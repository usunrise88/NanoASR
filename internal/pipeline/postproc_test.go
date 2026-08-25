package pipeline

import (
	"context"
	"testing"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/vad"
)

// ITN reaches the transcript, rewrites the words, and keeps every word anchored
// to the audio it came from.
func TestPostprocNormalisesNumbers(t *testing.T) {
	rec := &fakeRecognizer{
		unit: "bpe", caps: core.Capabilities{WordTimestamps: true},
		results: []asr.Recognition{
			timedRecognition("двадцать пять рублей",
				[]string{"▁двадцать", "▁пять", "▁рублей"},
				[]float32{0.0, 0.5, 1.0},
				[]float32{0.4, 0.4, 0.4}),
		},
	}
	h := newHarness(t, silence(2), []vad.Segment{segment(0, 2)}, rec, Options{})

	got, err := h.pipeline.Transcribe(context.Background(),
		core.Request{Audio: &fakeSource{}, ITN: true, Language: "ru"})
	if err != nil {
		t.Fatal(err)
	}

	if got.Text != "25 руб." {
		t.Errorf("text = %q, want %q", got.Text, "25 руб.")
	}
	if hasWarning(got.Warnings, "itn_unavailable") {
		t.Errorf("warnings %+v say normalisation was unavailable", got.Warnings)
	}

	words := got.Words()
	if len(words) != 1 {
		t.Fatalf("got %d words, want the three merged into one", len(words))
	}
	// The rewritten word has to span the run it replaced, or the player would
	// seek to the wrong place.
	if words[0].Start != 0 {
		t.Errorf("word starts at %v, want the first input's 0", words[0].Start)
	}
	if words[0].Original != "двадцать пять рублей" {
		t.Errorf("Original = %q, want what was said", words[0].Original)
	}
	// The segment still contains its own words.
	if got.Segments[0].End < words[0].End {
		t.Errorf("segment ends at %v before its own word at %v",
			got.Segments[0].End, words[0].End)
	}
	if _, ok := got.Stats.StagesMS["post"]; !ok {
		t.Error("stages_ms has no post entry")
	}
}

// Without itn=true nothing is rewritten: post-processing is opt-in.
func TestPostprocLeavesTextAloneUnlessAsked(t *testing.T) {
	rec := &fakeRecognizer{
		unit: "bpe", caps: core.Capabilities{WordTimestamps: true},
		results: []asr.Recognition{
			timedRecognition("двадцать пять рублей",
				[]string{"▁двадцать", "▁пять", "▁рублей"},
				[]float32{0.0, 0.5, 1.0},
				[]float32{0.4, 0.4, 0.4}),
		},
	}
	h := newHarness(t, silence(2), []vad.Segment{segment(0, 2)}, rec, Options{})

	got, err := h.pipeline.Transcribe(context.Background(),
		core.Request{Audio: &fakeSource{}, Language: "ru"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "двадцать пять рублей" {
		t.Errorf("text = %q, want the words untouched", got.Text)
	}
}

// A locale with no rules is reported rather than silently skipped.
func TestPostprocWarnsOnAnUnknownLocale(t *testing.T) {
	rec := &fakeRecognizer{
		unit: "bpe", caps: core.Capabilities{WordTimestamps: true},
		results: []asr.Recognition{
			timedRecognition("да", []string{"▁да"}, []float32{0}, []float32{0.2}),
		},
	}
	h := newHarness(t, silence(2), []vad.Segment{segment(0, 1)}, rec, Options{})

	got, err := h.pipeline.Transcribe(context.Background(),
		core.Request{Audio: &fakeSource{}, ITN: true, Language: "de"})
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(got.Warnings, "itn_unavailable") {
		t.Errorf("warnings %+v should name the missing locale", got.Warnings)
	}
}
