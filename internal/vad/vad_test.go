package vad

import (
	"context"
	"testing"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
)

func seg(start, length int) Segment {
	return Segment{StartSample: start, Samples: make([]float32, length)}
}

func TestSilencesIsTheComplementOfSpeech(t *testing.T) {
	// 16 kHz, 3 seconds: speech 0.5–1.0 and 2.0–2.5, silence elsewhere.
	const rate = 16000
	segs := []Segment{seg(8000, 8000), seg(32000, 8000)}

	got := Silences(segs, 3*rate, rate, 300)
	want := []core.Silence{{Start: 0, End: 0.5}, {Start: 1.0, End: 2.0}, {Start: 2.5, End: 3.0}}

	if len(got) != len(want) {
		t.Fatalf("got %d gaps %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if !near(got[i].Start, want[i].Start) || !near(got[i].End, want[i].End) {
			t.Errorf("gap %d = [%.3f,%.3f], want [%.3f,%.3f]",
				i, got[i].Start, got[i].End, want[i].Start, want[i].End)
		}
	}
}

func TestSilencesIgnoresShortGaps(t *testing.T) {
	const rate = 16000
	// A 100 ms gap between two utterances is a breath, not a silence region.
	segs := []Segment{seg(0, 16000), seg(17600, 16000)}

	got := Silences(segs, 2*rate+1600, rate, 300)
	for _, s := range got {
		if s.End-s.Start < 0.3 {
			t.Errorf("reported a %.3fs gap below the 300 ms floor", s.End-s.Start)
		}
	}
}

func TestSilencesHandlesEdgeCases(t *testing.T) {
	if got := Silences(nil, 0, 16000, 300); got != nil {
		t.Errorf("empty recording produced %v", got)
	}
	if got := Silences(nil, 16000, 0, 300); got != nil {
		t.Errorf("zero sample rate produced %v", got)
	}
	// No speech at all: the whole recording is one silence.
	if got := Silences(nil, 16000, 16000, 300); len(got) != 1 || !near(got[0].End, 1.0) {
		t.Errorf("silent recording produced %+v, want one full-length gap", got)
	}
}

func TestSpeechRatio(t *testing.T) {
	if got := SpeechRatio([]Segment{seg(0, 8000)}, 16000); !near(got, 0.5) {
		t.Errorf("SpeechRatio = %.3f, want 0.5", got)
	}
	if got := SpeechRatio(nil, 0); got != 0 {
		t.Errorf("SpeechRatio on an empty recording = %.3f, want 0", got)
	}
}

func TestDisabledTreatsRecordingAsOneUtterance(t *testing.T) {
	pcm := audio.PCM{Samples: make([]float32, 16000), SampleRate: 16000}

	got, err := Disabled{}.Segment(context.Background(), pcm)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].StartSample != 0 || len(got[0].Samples) != 16000 {
		t.Fatalf("got %d segments %+v, want one covering the whole recording", len(got), got)
	}
}

func near(a, b float64) bool {
	d := a - b
	return d < 1e-6 && d > -1e-6
}
