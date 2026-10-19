//go:build integration

package tse

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/ortenv"
	"github.com/usunrise88/nanoasr/internal/tse/voxlock"
)

var root = filepath.Join("..", "..")

func engine(t testing.TB) *Engine {
	t.Helper()
	vdir := filepath.Join(root, ".models", "voxlock-int8@2026-10-12")
	cpath := filepath.Join(root, ".models", "campplus-sv-zh-en@16k-common-advanced",
		"3dspeaker_speech_campplus_sv_zh_en_16k-common_advanced.onnx")
	for _, p := range []string{filepath.Join(vdir, "chunk_int8.onnx"), cpath} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("no %s (run scripts/fetch-dev-models.sh)", p)
		}
	}
	m, err := voxlock.Load(voxlock.Files{Enroll: filepath.Join(vdir, "enroll.onnx"), Chunk: filepath.Join(vdir, "chunk_int8.onnx")},
		ortenv.Tuning{IntraOpThreads: 2})
	if err != nil {
		t.Fatal(err)
	}
	emb, err := NewSherpaEmbedder(cpath, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(); emb.Close() })
	return &Engine{Model: m, Embedder: emb, Selection: DefaultSelection(), Gate: DefaultGate()}
}

func wav(t testing.TB, name string) []float32 {
	t.Helper()
	f, err := os.Open(filepath.Join(root, "testdata", "audio", name))
	if err != nil {
		t.Skipf("no %s (run scripts/fetch-testdata.sh): %v", name, err)
	}
	defer func() { _ = f.Close() }()
	pcm, err := audio.NewWAVDecoder().Decode(context.Background(), f, audio.Options{TargetSampleRate: SampleRate})
	if err != nil {
		t.Fatal(err)
	}
	return pcm[0].Samples
}

func db(a, b float64) float64 { return 20 * math.Log10(max(a, 1e-9)/max(b, 1e-9)) }

// ru-2spk.wav is one voice, then the same clip pitch-shifted into a second
// one: the case the gate exists for, where the chosen voice falls silent and
// the other carries on alone. Isolating either must keep it and remove the
// other.
func TestIsolatesEitherVoiceOfASequentialDialogue(t *testing.T) {
	e := engine(t)
	x := wav(t, "ru-2spk.wav")
	half := float64(len(x)) / SampleRate / 2
	a := Speaker{ID: "spk_0", Spans: []Span{{0, half}}}
	b := Speaker{ID: "spk_1", Spans: []Span{{half, 2 * half}}}

	voices, err := e.Samples(context.Background(), x, []Speaker{a, b})
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range voices {
		if v.Err != nil {
			t.Fatalf("%s: %v", v.Speaker.ID, v.Err)
		}
		own, other := v.Speaker.Spans[0], voices[1-i].Speaker.Spans[0]
		t.Logf("%s: %s sample at %.2f s, similarity %.2f", v.Speaker.ID, v.Sample.Kind, v.Sample.Start, v.Sample.Similarity)

		start := time.Now()
		// No keep spans: the embeddings alone have to tell the voices apart.
		others := [][]float32{voices[1-i].Sample.Embedding}
		y, err := e.Extract(context.Background(), x, v.Sample, others, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		took := time.Since(start)
		if len(y) != len(x) {
			t.Fatalf("%d samples out for %d in", len(y), len(x))
		}
		kept := db(rms(slice(y, own.Start+0.5, own.End-0.5)), rms(slice(x, own.Start+0.5, own.End-0.5)))
		removed := db(rms(slice(y, other.Start+0.5, other.End-0.5)), rms(slice(x, other.Start+0.5, other.End-0.5)))
		t.Logf("%s: own voice %+.1f dB, the other %+.1f dB, %.2fx real time",
			v.Speaker.ID, kept, removed, took.Seconds()/(float64(len(x))/SampleRate))
		if kept < -3 {
			t.Errorf("%s: its own voice lost %.1f dB", v.Speaker.ID, -kept)
		}
		if removed > -20 {
			t.Errorf("%s: the other voice is only %.1f dB down", v.Speaker.ID, removed)
		}
	}
}

// Embeddings from several goroutines at once are the ones one goroutine gets.
func TestEmbedderIsSafeForConcurrentUse(t *testing.T) {
	e := engine(t)
	x := wav(t, "ru-16k.wav")[:3*SampleRate]
	want, err := e.Embedder.Embed(x)
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			got, err := e.Embedder.Embed(x)
			if err == nil && cosine(got, want) < 0.99999 {
				err = os.ErrInvalid
			}
			errs <- err
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

// Real dialogue: every turn of the chosen speaker must come back whole after
// the other one has talked. Carried across the other speaker, the extractor's
// state lets the chosen voice go; on this dialogue that muted the first second
// of 17 of Dima's 52 turns and 7 of Masha's 31 by more than 6 dB, and starting
// each turn from a fresh state is what this test holds in place.
//
// The fixture is scripts/fetch-diar-eval.sh with UTTERANCES=40; it is not in
// CI, so the test skips without it.
func TestEveryTurnComesBackWhole(t *testing.T) {
	e := engine(t)
	base := filepath.Join(root, "testdata", "audio", "diar-masha_dima_part1-40")
	turns, err := readRTTM(base + ".rttm")
	if err != nil {
		t.Skipf("no dialogue fixture; run UTTERANCES=40 ./scripts/fetch-diar-eval.sh (%v)", err)
	}
	x := wav(t, filepath.Base(base)+".wav")

	var speakers []Speaker
	for _, id := range []string{"D", "M"} {
		speakers = append(speakers, Speaker{ID: id, Spans: turns[id]})
	}
	voices, err := e.Samples(context.Background(), x, speakers)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range voices {
		if v.Err != nil {
			t.Fatal(v.Err)
		}
		other := voices[1-i]
		y, err := e.Extract(context.Background(), x, v.Sample, [][]float32{other.Sample.Embedding}, v.Speaker.Spans, nil)
		if err != nil {
			t.Fatal(err)
		}
		lost := 0
		regs := merge(v.Speaker.Spans)
		for j := 0; j < len(regs); j++ {
			// Turns a breath apart are one turn, as regions joins them.
			st := regs[j].Start
			for j+1 < len(regs) && regs[j+1].Start-regs[j].End < regionMerge {
				j++
			}
			on := min(regs[j].End, st+1)
			if db(rms(slice(y, st, on)), rms(slice(x, st, on))) < -6 {
				lost++
			}
		}
		down := db(rms(slice(y, other.Speaker.Spans[0].Start, other.Speaker.Spans[0].End)),
			rms(slice(x, other.Speaker.Spans[0].Start, other.Speaker.Spans[0].End)))
		t.Logf("%s: %d turn starts lost more than 6 dB; the other speaker's first turn %+.1f dB", v.Speaker.ID, lost, down)
		// One short word at a hand-over may still go to the gate.
		if lost > 1 {
			t.Errorf("%s: %d turns lost their start", v.Speaker.ID, lost)
		}
	}
}

// readRTTM reads speaker turns from a reference.
func readRTTM(path string) (map[string][]Span, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string][]Span{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 8 || f[0] != "SPEAKER" {
			continue
		}
		st, err1 := strconv.ParseFloat(f[3], 64)
		d, err2 := strconv.ParseFloat(f[4], 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("bad line %q", line)
		}
		out[f[7]] = append(out[f[7]], Span{st, st + d})
	}
	return out, nil
}
