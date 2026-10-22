package vad

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
)

// TestDecideMatchesStateMachine drives the decision port with probabilities
// whose segments can be worked out by hand.
func TestDecideMatchesStateMachine(t *testing.T) {
	cfg := Config{SampleRate: 16000}.withDefaults()
	// 2 s silence, 3 s speech, 2 s silence: 512-sample windows.
	win := func(sec float64) int { return int(sec * 16000 / sileroShift) }
	var probs []float32
	for range win(2) {
		probs = append(probs, 0.05)
	}
	for range win(3) {
		probs = append(probs, 0.95)
	}
	for range win(2) {
		probs = append(probs, 0.05)
	}
	x := make([]float32, len(probs)*sileroShift+sileroWindow)
	segs := decide(cfg, probs, x)
	if len(segs) != 1 {
		t.Fatalf("got %d segments, want 1", len(segs))
	}
	s := segs[0]
	startSec := float64(s.StartSample) / 16000
	endSec := float64(s.EndSample()) / 16000
	// Speech starts at 2.0 s; the detector backs up by two windows and the
	// minimum speech length, and ends min_silence before it gave up.
	if math.Abs(startSec-(2.0-2*512.0/16000)) > 0.1 {
		t.Errorf("start %.3f s", startSec)
	}
	if endSec < 4.9 || endSec > 5.1 {
		t.Errorf("end %.3f s", endSec)
	}

	// An utterance with no pause is cut once it outgrows max_speech_duration.
	probs = probs[:0]
	for range win(50) {
		// Speech by the normal threshold, but below the forced one and below
		// its own speaking threshold (0.9 - 0.15), so a cut can happen.
		probs = append(probs, 0.7)
	}
	x = make([]float32, len(probs)*sileroShift+sileroWindow)
	segs = decide(cfg, probs, x)
	if len(segs) < 2 {
		t.Fatalf("a 50 s monologue came back as %d segment(s)", len(segs))
	}
	for _, s := range segs {
		if d := float64(len(s.Samples)) / 16000; d > float64(cfg.MaxSpeechSec)+1 {
			t.Errorf("segment of %.1f s", d)
		}
	}
}

// TestCoreMLMatchesSherpa runs both detectors over one recording and compares
// their segments. It needs the converted model (NANOASR_COREML_VAD), the ONNX
// one sherpa-onnx loads (NANOASR_SILERO_ONNX) and a 16 kHz mono WAV
// (NANOASR_VAD_WAV).
func TestCoreMLMatchesSherpa(t *testing.T) {
	mlPath, onnxPath, wav := os.Getenv("NANOASR_COREML_VAD"), os.Getenv("NANOASR_SILERO_ONNX"), os.Getenv("NANOASR_VAD_WAV")
	if mlPath == "" || onnxPath == "" || wav == "" {
		t.Skip("NANOASR_COREML_VAD, NANOASR_SILERO_ONNX and NANOASR_VAD_WAV are not all set")
	}
	f, err := os.Open(wav)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pcms, err := audio.NewWAVDecoder().Decode(context.Background(), f,
		audio.Options{TargetSampleRate: 16000, ChannelMode: core.ChannelDownmix})
	if err != nil {
		t.Fatal(err)
	}
	pcm := pcms[0]

	ml, err := NewCoreML(Config{}, mlPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ml.Close()
	ref, err := NewPool(Config{ModelPath: onnxPath}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer ref.Close()

	t0 := time.Now()
	got, err := ml.Segment(context.Background(), pcm)
	if err != nil {
		t.Fatal(err)
	}
	tML := time.Since(t0)
	t0 = time.Now()
	want, err := ref.Segment(context.Background(), pcm)
	if err != nil {
		t.Fatal(err)
	}
	tRef := time.Since(t0)
	t.Logf("%.0f s of audio: CoreML %d segments in %v, sherpa %d in %v",
		pcm.Duration(), len(got), tML.Round(time.Millisecond), len(want), tRef.Round(time.Millisecond))

	// Segments match when their boundaries agree to within a window or two;
	// fp16 moves a probability across the threshold now and then, which can
	// merge or split a pair, so a few percent may differ.
	matched := 0
	for _, w := range want {
		for _, g := range got {
			if abs(g.StartSample-w.StartSample) <= 2*sileroShift && abs(g.EndSample()-w.EndSample()) <= 2*sileroShift {
				matched++
				break
			}
		}
	}
	speech := func(ss []Segment) float64 {
		n := 0
		for _, s := range ss {
			n += len(s.Samples)
		}
		return float64(n) / 16000
	}
	t.Logf("%d of %d sherpa segments matched; speech %.1f s vs %.1f s", matched, len(want), speech(got), speech(want))
	if float64(matched) < 0.9*float64(len(want)) {
		t.Errorf("only %d of %d segments match", matched, len(want))
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
