package words

import (
	"testing"

	"github.com/usunrise88/nanoasr/internal/asr"
)

func rec(tokens []string, ts, dur, lp []float32) asr.Recognition {
	return asr.Recognition{Tokens: tokens, Timestamps: ts, Durations: dur, LogProbs: lp}
}

func TestAssembleBPE(t *testing.T) {
	r := rec(
		[]string{"▁здрав", "ствуй", "те", "▁это", "▁ком", "пания"},
		[]float32{0.00, 0.28, 0.52, 0.80, 1.10, 1.36},
		[]float32{0.28, 0.24, 0.20, 0.22, 0.26, 0.30},
		nil,
	)
	got := Assemble(r, Options{ModelingUnit: UnitBPE, SegmentStart: 10, SegmentEnd: 12})

	want := []struct {
		text       string
		start, end float64
	}{
		{"здравствуйте", 10.00, 10.72},
		{"это", 10.80, 11.02},
		{"компания", 11.10, 11.66},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d words %+v, want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if got[i].Word != w.text {
			t.Errorf("word %d: got %q, want %q", i, got[i].Word, w.text)
		}
		if !near(got[i].Start, w.start) || !near(got[i].End, w.end) {
			t.Errorf("word %d %q: got [%.3f,%.3f], want [%.3f,%.3f]",
				i, w.text, got[i].Start, got[i].End, w.start, w.end)
		}
	}
}

func TestAssembleInvariants(t *testing.T) {
	r := rec(
		[]string{"▁one", "▁two", "▁three"},
		[]float32{0.0, 0.5, 0.9},
		[]float32{0.9, 0.5, 0.4}, // deliberately overlapping durations
		nil,
	)
	got := Assemble(r, Options{ModelingUnit: UnitBPE, SegmentStart: 0, SegmentEnd: 1.3})

	for i, w := range got {
		if !(w.Start >= 0 && w.Start <= w.End && w.End <= 1.3) {
			t.Errorf("word %d %q violates bounds: [%.3f,%.3f]", i, w.Word, w.Start, w.End)
		}
		if i+1 < len(got) && got[i].End > got[i+1].Start {
			t.Errorf("word %d overlaps %d: %.3f > %.3f", i, i+1, got[i].End, got[i+1].Start)
		}
	}
}

func TestAssembleMergesPunctuation(t *testing.T) {
	r := rec(
		[]string{"▁да", "▁,", "▁конечно"},
		[]float32{0.0, 0.30, 0.35},
		[]float32{0.28, 0.02, 0.50},
		nil,
	)
	got := Assemble(r, Options{ModelingUnit: UnitBPE, SegmentEnd: 1})
	if len(got) != 2 {
		t.Fatalf("got %d words %+v, want 2", len(got), got)
	}
	if got[0].Word != "да," {
		t.Errorf("got %q, want %q", got[0].Word, "да,")
	}
}

func TestAssembleSkipsSpecialTokens(t *testing.T) {
	r := rec(
		[]string{"<sos/eos>", "▁hello", "<blk>", "▁world"},
		[]float32{0.0, 0.1, 0.4, 0.5},
		[]float32{0.0, 0.25, 0.0, 0.30},
		nil,
	)
	got := Assemble(r, Options{ModelingUnit: UnitBPE, SegmentEnd: 1})
	if Text(got) != "hello world" {
		t.Fatalf("got %q, want %q", Text(got), "hello world")
	}
}

func TestAssembleCJKCharIsOneWordPerToken(t *testing.T) {
	r := rec(
		[]string{"今", "天", "好"},
		[]float32{0.0, 0.2, 0.4},
		[]float32{0.2, 0.2, 0.2},
		nil,
	)
	got := Assemble(r, Options{ModelingUnit: UnitCJKChar, SegmentEnd: 1})
	if len(got) != 3 {
		t.Fatalf("got %d words, want 3", len(got))
	}
}

func TestAssembleWithoutTimestampsReturnsNil(t *testing.T) {
	r := asr.Recognition{Text: "no timings here"}
	if got := Assemble(r, Options{ModelingUnit: UnitBPE}); got != nil {
		t.Fatalf("got %+v, want nil so the caller falls back to segment timing", got)
	}
}

func TestConfidenceIsGeometricMean(t *testing.T) {
	r := rec(
		[]string{"▁a", "b"},
		[]float32{0, 0.1},
		[]float32{0.1, 0.1},
		[]float32{-0.2, -0.4},
	)
	got := Assemble(r, Options{ModelingUnit: UnitBPE, SegmentEnd: 1, WithConfidence: true})
	if len(got) != 1 {
		t.Fatalf("got %d words, want 1", len(got))
	}
	// exp((-0.2 + -0.4)/2) = exp(-0.3) ≈ 0.7408
	if got[0].Confidence < 0.73 || got[0].Confidence > 0.75 {
		t.Errorf("confidence = %.4f, want ≈0.7408", got[0].Confidence)
	}
}

func near(a, b float64) bool {
	d := a - b
	return d < 1e-6 && d > -1e-6
}

// GigaAM's Russian CTC uses a character vocabulary whose separator is a
// literal space with a frame of its own. Folding that frame into the next word
// would bias every word start early and leave a leading space in the text.
func TestAssembleCharUnitDropsSeparatorFrames(t *testing.T) {
	r := rec(
		[]string{" ", "д", "а", " ", "н", "е", "т"},
		[]float32{0.00, 0.08, 0.16, 0.24, 0.32, 0.40, 0.48},
		[]float32{0.08, 0.08, 0.08, 0.08, 0.08, 0.08, 0.08},
		nil,
	)
	got := Assemble(r, Options{ModelingUnit: UnitChar, SegmentEnd: 1})

	if len(got) != 2 {
		t.Fatalf("got %d words %+v, want 2", len(got), got)
	}
	if got[0].Word != "да" || got[1].Word != "нет" {
		t.Fatalf("got %q and %q", got[0].Word, got[1].Word)
	}
	// The word starts at its first letter, not at the space before it.
	if !near(got[0].Start, 0.08) {
		t.Errorf("first word starts at %.3f, want 0.08 — the separator frame leaked in", got[0].Start)
	}
	if !near(got[1].Start, 0.32) {
		t.Errorf("second word starts at %.3f, want 0.32", got[1].Start)
	}
	if !near(got[0].End, 0.24) {
		t.Errorf("first word ends at %.3f, want 0.24", got[0].End)
	}
}

func TestAssembleCharUnitWithoutLeadingSeparator(t *testing.T) {
	r := rec([]string{"д", "а"}, []float32{0.00, 0.08}, []float32{0.08, 0.08}, nil)

	got := Assemble(r, Options{ModelingUnit: UnitChar, SegmentEnd: 1})
	if len(got) != 1 || got[0].Word != "да" || !near(got[0].Start, 0) {
		t.Fatalf("got %+v, want one word starting at 0", got)
	}
}
