//go:build integration

package voxlock

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/ortenv"
)

var root = filepath.Join("..", "..", "..")

// ModelDir is where the graphs live: VOXLOCK_MODEL_DIR, or the registry's
// layout under .models. Tests that need them skip without them.
func ModelDir(t testing.TB) string {
	t.Helper()
	dir := os.Getenv("VOXLOCK_MODEL_DIR")
	if dir == "" {
		dir = filepath.Join(root, ".models", "voxlock-int8@2026-10-12")
	}
	for _, f := range []string{"enroll.onnx", "chunk_int8.onnx"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Skipf("no %s in %s (set VOXLOCK_MODEL_DIR or run scripts/fetch-dev-models.sh)", f, dir)
		}
	}
	return dir
}

func loadModel(t testing.TB, threads int) *Model {
	t.Helper()
	dir := ModelDir(t)
	m, err := Load(Files{Enroll: filepath.Join(dir, "enroll.onnx"), Chunk: filepath.Join(dir, "chunk_int8.onnx")},
		ortenv.Tuning{IntraOpThreads: threads})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func readClip(t testing.TB) []float32 {
	t.Helper()
	f, err := os.Open(filepath.Join(root, "testdata", "audio", "ru-16k.wav"))
	if err != nil {
		t.Skipf("no reference clip (run scripts/fetch-testdata.sh): %v", err)
	}
	defer func() { _ = f.Close() }()
	pcm, err := audio.NewWAVDecoder().Decode(context.Background(), f, audio.Options{TargetSampleRate: SampleRate})
	if err != nil {
		t.Fatal(err)
	}
	return pcm[0].Samples
}

// goldenInput is golden_voxlock.py's: the first 3 s of the clip as the voice,
// the rest with the clip reversed laid over it.
func goldenInput(clip []float32, interferer float32) (sample, mix []float32) {
	sample = clip[:EnrollSamples]
	rest := clip[EnrollSamples:]
	mix = make([]float32, len(rest))
	for i := range rest {
		mix[i] = rest[i] + interferer*clip[len(clip)-1-i]
	}
	return sample, mix
}

// The Go port against the Python reference it was ported from, on the same
// weights: what differs is the onnxruntime build (sherpa-onnx's against
// pip's), not the signal path. That alone is not nothing for an int8 graph:
// the reference on onnxruntime 1.27 and on 1.31 agree to 47.7 dB, and this
// port on sherpa's 1.27.1 agrees with the 1.31 golden to 45 dB. A padding or
// alignment mistake would land near 0 dB, so 35 is a bound with room for
// another build and nothing to hide a bug in.
func TestMatchesTheReference(t *testing.T) {
	m := loadModel(t, 2)
	dir := filepath.Join(root, "testdata", "golden", "voxlock")
	var meta struct {
		Samples        int     `json:"samples"`
		Decimate       int     `json:"decimate"`
		InterfererGain float32 `json:"interferer_gain"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "output.f32"))
	if err != nil {
		t.Fatal(err)
	}
	want := make([]float32, len(raw)/4)
	if err := binary.Read(bytes.NewReader(raw), binary.LittleEndian, want); err != nil {
		t.Fatal(err)
	}

	sample, mix := goldenInput(readClip(t), meta.InterfererGain)
	if len(mix) != meta.Samples {
		t.Fatalf("the clip gives %d samples, the golden was made from %d: regenerate it", len(mix), meta.Samples)
	}
	v, err := m.Enroll(sample)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Isolate(context.Background(), mix, v, nil)
	if err != nil {
		t.Fatal(err)
	}

	var sig, diff float64
	for i, w := range want {
		g := got[i*meta.Decimate]
		sig += float64(w) * float64(w)
		diff += float64(g-w) * float64(g-w)
	}
	snr := 10 * math.Log10(sig/max(diff, 1e-30))
	t.Logf("Go against the reference: %.1f dB", snr)
	if snr < 35 {
		t.Errorf("the Go output matches the reference to only %.1f dB", snr)
	}
}

// One model serves every isolation at once; each stream carries its own state,
// so concurrent streams must compute what one alone does.
func TestConcurrentStreamsMatchSerial(t *testing.T) {
	m := loadModel(t, 1)
	sample, mix := goldenInput(readClip(t), 0.7)
	mix = mix[:4*SampleRate]
	v, err := m.Enroll(sample)
	if err != nil {
		t.Fatal(err)
	}
	alone, err := m.Isolate(context.Background(), mix, v, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	outs := make([][]float32, 4)
	errs := make([]error, 4)
	for i := range outs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = m.Isolate(context.Background(), mix, v, nil)
		}()
	}
	wg.Wait()
	for i, o := range outs {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		for j := range o {
			if o[j] != alone[j] {
				t.Fatalf("stream %d differs at sample %d: %g against %g", i, j, o[j], alone[j])
			}
		}
	}
}

// A wrong-length sample fails inside the graph's attention; Enroll refuses it
// before that, with a reason.
func TestRealGraphRejectsOtherLengths(t *testing.T) {
	m := loadModel(t, 1)
	if _, err := m.Enroll(make([]float32, 4*SampleRate)); err == nil {
		t.Error("a 4 s sample was accepted")
	}
}
