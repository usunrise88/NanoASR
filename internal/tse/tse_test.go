package tse

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/usunrise88/nanoasr/internal/tse/voxlock"
)

// tones is an embedder for voices that are pure tones: the embedding is how
// much of each tone a stretch holds. Two tones are as different as two voices
// get, which is what makes the decisions below checkable by hand.
type tones struct {
	freqs []float64
	calls int
}

func (t *tones) Embed(s []float32) ([]float32, error) {
	t.calls++
	out := make([]float32, len(t.freqs))
	for i, f := range t.freqs {
		// Goertzel power at f.
		w := 2 * math.Pi * f / SampleRate
		coeff := 2 * math.Cos(w)
		var s1, s2 float64
		for _, x := range s {
			s0 := float64(x) + coeff*s1 - s2
			s2, s1 = s1, s0
		}
		out[i] = float32(math.Sqrt(s1*s1 + s2*s2 - coeff*s1*s2))
	}
	return normalise(out), nil
}

const (
	toneA = 200.0
	toneB = 700.0
)

// recording lays tones out over time: each span is one voice speaking.
func recording(seconds float64, parts ...struct {
	f    float64
	span Span
}) []float32 {
	a := make([]float32, int(seconds*SampleRate))
	for _, p := range parts {
		for i := int(p.span.Start * SampleRate); i < int(p.span.End*SampleRate) && i < len(a); i++ {
			a[i] += float32(0.3 * math.Sin(2*math.Pi*p.f*float64(i)/SampleRate))
		}
	}
	return a
}

type part = struct {
	f    float64
	span Span
}

func TestSpanArithmetic(t *testing.T) {
	m := merge([]Span{{3, 4}, {0, 1}, {0.5, 2}, {5, 5}})
	if len(m) != 2 || m[0] != (Span{0, 2}) || m[1] != (Span{3, 4}) {
		t.Fatalf("merge = %v", m)
	}
	d := subtract([]Span{{0, 10}}, []Span{{1, 2}, {4, 5}, {9, 12}})
	want := []Span{{0, 1}, {2, 4}, {5, 9}}
	if len(d) != len(want) {
		t.Fatalf("subtract = %v", d)
	}
	for i := range want {
		if d[i] != want[i] {
			t.Fatalf("subtract = %v, want %v", d, want)
		}
	}
	if c := coverage(want, Span{0.5, 6}); math.Abs(c-3.5) > 1e-9 {
		t.Errorf("coverage = %g, want 3.5", c)
	}
}

// The sample comes from where only the chosen voice speaks, and from far
// enough away from the other one that no hand-over bleeds into it.
func TestSampleAvoidsTheOtherVoice(t *testing.T) {
	a := Speaker{ID: "spk_0", Spans: []Span{{0, 2}, {4, 9}}}
	b := Speaker{ID: "spk_1", Spans: []Span{{1.5, 4}}}
	audio := recording(9, part{toneA, Span{0, 2}}, part{toneA, Span{4, 9}}, part{toneB, Span{1.5, 4}})
	emb := &tones{freqs: []float64{toneA, toneB}}

	s, err := ChooseSample(audio, a, []Speaker{a, b}, emb, DefaultSelection())
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind != SampleWindow {
		t.Fatalf("kind %s, want a window", s.Kind)
	}
	if s.Start < 4+DefaultSelection().Margin-1e-9 || s.Start+3 > 9+1e-9 {
		t.Errorf("sample at %.2f s overlaps the other voice or its margin", s.Start)
	}
	if len(s.Audio) != voxlock.EnrollSamples {
		t.Errorf("sample is %d samples", len(s.Audio))
	}
	if s.Similarity < 0.99 {
		t.Errorf("similarity %.3f for a pure voice", s.Similarity)
	}
	if math.Abs(s.CleanSpeech-(1.35+4.85)) > 0.01 {
		t.Errorf("clean speech %.2f s", s.CleanSpeech)
	}
}

// A voice that is never alone for 3 seconds is still a voice: its clean
// pieces make the sample.
func TestShortTurnsAreAssembled(t *testing.T) {
	a := Speaker{ID: "a", Spans: []Span{{0, 1.2}, {2, 3.2}, {4, 5.2}}}
	b := Speaker{ID: "b", Spans: []Span{{1.2, 2}, {3.2, 4}, {5.2, 6}}}
	audio := recording(6, part{toneA, Span{0, 1.2}}, part{toneA, Span{2, 3.2}}, part{toneA, Span{4, 5.2}},
		part{toneB, Span{1.2, 2}}, part{toneB, Span{3.2, 4}}, part{toneB, Span{5.2, 6}})
	s, err := ChooseSample(audio, a, []Speaker{a, b}, &tones{freqs: []float64{toneA, toneB}}, DefaultSelection())
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind != SampleAssembled || len(s.Audio) != voxlock.EnrollSamples {
		t.Fatalf("kind %s, %d samples", s.Kind, len(s.Audio))
	}
	if s.Similarity < 0.99 {
		t.Errorf("the assembled sample sounds like someone else: %.3f", s.Similarity)
	}
}

func TestTooLittleSpeechIsRefused(t *testing.T) {
	a := Speaker{ID: "a", Spans: []Span{{0, 1}}}
	b := Speaker{ID: "b", Spans: []Span{{1, 5}}}
	audio := recording(5, part{toneA, Span{0, 1}}, part{toneB, Span{1, 5}})
	_, err := ChooseSample(audio, a, []Speaker{a, b}, &tones{freqs: []float64{toneA, toneB}}, DefaultSelection())
	var short ErrTooLittleSpeech
	if !errors.As(err, &short) || short.Clean >= 1 {
		t.Fatalf("err = %v, want too little speech with under 1 s clean", err)
	}
}

// An hour of one voice costs a bounded number of embeddings.
func TestEmbeddingsAreBounded(t *testing.T) {
	a := Speaker{ID: "a", Spans: []Span{{0, 600}}}
	audio := recording(600, part{toneA, Span{0, 600}})
	emb := &tones{freqs: []float64{toneA, toneB}}
	sel := DefaultSelection()
	if _, err := ChooseSample(audio, a, nil, emb, sel); err != nil {
		t.Fatal(err)
	}
	if limit := sel.MaxCandidates + sel.MaxCentroidSpans; emb.calls > limit {
		t.Errorf("%d embeddings for one voice; the bound is %d", emb.calls, limit)
	}
}

// What the gate exists for: the extractor's output while the chosen voice is
// silent and another is speaking is muted, and the chosen voice is not.
func TestGateMutesTheOtherVoice(t *testing.T) {
	y := recording(6, part{toneA, Span{0, 2}}, part{toneB, Span{2, 4}}, part{toneA, Span{4, 6}})
	emb := &tones{freqs: []float64{toneA, toneB}}
	ref, _ := emb.Embed(recording(1, part{toneA, Span{0, 1}}))
	out, err := DefaultGate().Apply(context.Background(), slices.Clone(y), ref, emb, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Away from the edges, where the decision window straddles both voices.
	if r := rms(slice(out, 2.8, 3.2)); r > 1e-4 {
		t.Errorf("the other voice survives at RMS %g", r)
	}
	for _, w := range []Span{{0.2, 1.2}, {4.8, 5.8}} {
		if r, want := rms(slice(out, w.Start, w.End)), rms(slice(y, w.Start, w.End)); math.Abs(r-want) > 1e-4 {
			t.Errorf("the chosen voice at %v went from RMS %g to %g", w, want, r)
		}
	}
}

// Inside the chosen speaker's own turns the gate defers to the diarizer.
func TestGateKeepsTheSpeakersOwnTurns(t *testing.T) {
	y := recording(4, part{toneB, Span{0, 4}})
	emb := &tones{freqs: []float64{toneA, toneB}}
	ref, _ := emb.Embed(recording(1, part{toneA, Span{0, 1}}))
	g := DefaultGate()
	g.Keep = []Span{{1, 3}}
	out, err := g.Apply(context.Background(), slices.Clone(y), ref, emb, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r, want := rms(slice(out, 1.2, 2.8)), rms(slice(y, 1.2, 2.8)); math.Abs(r-want) > 1e-4 {
		t.Errorf("a kept turn went from RMS %g to %g", want, r)
	}
	if r := rms(slice(out, 0, 0.8)); r > 1e-4 {
		t.Errorf("outside the kept turn the other voice survives at RMS %g", r)
	}
}

// A stretch that resembles the chosen voice somewhat but another voice more
// is the other voice: no single threshold makes that call for similar voices.
func TestGateGivesAStretchToTheNearestVoice(t *testing.T) {
	emb := &tones{freqs: []float64{toneA, toneB}}
	ref, _ := emb.Embed(recording(1, part{toneA, Span{0, 1}}))
	other, _ := emb.Embed(recording(1, part{toneB, Span{0, 1}}))
	// Mostly B with a third as much A: about 0.32 against A, 0.95 against B.
	y := recording(3, part{toneB, Span{0, 3}})
	for i := range y {
		y[i] += 0.1 * float32(math.Sin(2*math.Pi*toneA*float64(i)/SampleRate))
	}
	g := DefaultGate()
	if out, _ := g.Apply(context.Background(), slices.Clone(y), ref, emb, nil); rms(slice(out, 1, 2)) == 0 {
		t.Fatal("setup: without the other voice the stretch should pass the floor")
	}
	g.Others = [][]float32{other}
	out, err := g.Apply(context.Background(), slices.Clone(y), ref, emb, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := rms(slice(out, 1, 2)); r > 1e-4 {
		t.Errorf("a stretch nearer the other voice survives at RMS %g", r)
	}
}

func TestCrossfadeRampsAtEveryChange(t *testing.T) {
	const hop, fade = 100, 21
	y := make([]float32, 400)
	for i := range y {
		y[i] = 1
	}
	crossfade(y, []bool{true, false, false, true}, hop, fade)
	if y[0] != 1 || y[399] != 1 || y[150] != 0 || y[250] != 0 {
		t.Fatalf("steady stretches moved: %g %g %g %g", y[0], y[399], y[150], y[250])
	}
	// Down across 100, up across 300, each 21 samples wide and monotone.
	for i := 80; i < 120; i++ {
		if y[i+1] > y[i] {
			t.Fatalf("not falling at %d: %v", i, y[85:115])
		}
	}
	for i := 280; i < 320; i++ {
		if y[i+1] < y[i] {
			t.Fatalf("not rising at %d: %v", i, y[285:315])
		}
	}
	if y[89] != 1 || y[111] != 0 || y[100] <= 0 || y[100] >= 1 {
		t.Errorf("the ramp is not 21 samples wide around 100: %v", y[88:113])
	}
	if y[289] != 0 || y[311] != 1 {
		t.Errorf("the ramp is not 21 samples wide around 300: %v", y[288:313])
	}
}

func TestRegionsStartAtEveryTurn(t *testing.T) {
	sec := func(s float64) int { return int(s * SampleRate) }
	n := sec(20)
	// Two turns a breath apart are one; the third stands alone.
	got := regions(n, []Span{{2, 5}, {5.3, 8}, {12, 15}})
	want := []region{
		{feed: 0, start: 0, end: sec(2)},
		{feed: sec(1.5), start: sec(2), end: sec(8)},
		{feed: sec(7.5), start: sec(8), end: sec(12)},
		{feed: sec(11.5), start: sec(12), end: sec(15)},
		{feed: sec(14.5), start: sec(15), end: n},
	}
	if len(got) != len(want) {
		t.Fatalf("regions = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("region %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// No turns: one region, the whole recording, as one pass.
	if got := regions(n, nil); len(got) != 1 || got[0] != (region{0, 0, n}) {
		t.Errorf("without turns: %v", got)
	}
	// A turn from the very start to the very end.
	if got := regions(n, []Span{{0, 20}}); len(got) != 1 || got[0] != (region{0, 0, n}) {
		t.Errorf("one turn throughout: %v", got)
	}
}
