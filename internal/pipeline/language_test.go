package pipeline

import (
	"context"
	"testing"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/vad"
)

var multilingual = fakeRegistry{
	languages: []string{"multi"},
	locales:   []string{"en-US", "en-GB", "pt-BR", "pt-PT", "ru-RU"},
}

func transcribeIn(t *testing.T, reg fakeRegistry, language string) (*core.Result, *fakeRecognizer) {
	t.Helper()
	rec := &fakeRecognizer{
		unit: "bpe", caps: core.Capabilities{WordTimestamps: true},
		results: []asr.Recognition{timedRecognition("да", []string{"▁да"}, []float32{0}, []float32{0.2})},
	}
	h := newHarnessFor(t, reg, silence(2), []vad.Segment{segment(0, 1)}, rec, Options{})
	got, err := h.pipeline.Transcribe(context.Background(), core.Request{Audio: &fakeSource{}, Language: language})
	if err != nil {
		t.Fatal(err)
	}
	return got, rec
}

// The language is a per-call setting of the recogniser, resolved against the
// model's own locales before it gets there.
func TestAMultilingualModelIsToldTheResolvedLanguage(t *testing.T) {
	for _, tc := range []struct{ asked, told, reported string }{
		{"ru-RU", "ru-RU", "ru-RU"},
		{"pt", "pt-BR", "pt-BR"}, // a bare code is the first locale listed
		{"PT_pt", "pt-PT", "pt-PT"},
		{"auto", "auto", "auto"},
		// Nothing asked: the model chooses, and the result says so rather than
		// reporting "multi", which describes the model and not the audio.
		{"", "auto", "auto"},
	} {
		t.Run(tc.asked, func(t *testing.T) {
			got, rec := transcribeIn(t, multilingual, tc.asked)
			if len(rec.opts) != 1 || rec.opts[0].Language != tc.told {
				t.Errorf("recogniser was given %+v, want language %q", rec.opts, tc.told)
			}
			if got.Language != tc.reported {
				t.Errorf("result language = %q, want %q", got.Language, tc.reported)
			}
			if hasWarning(got.Warnings, "language_mismatch") {
				t.Errorf("unexpected warning: %+v", got.Warnings)
			}
		})
	}
}

// A language the model does not offer is warned about, not refused — the same
// choice every model-dependent option makes — and the model decodes in auto.
func TestAnUnofferedLanguageWarnsAndFallsBackToAuto(t *testing.T) {
	got, rec := transcribeIn(t, multilingual, "de-DE")
	if !hasWarning(got.Warnings, "language_mismatch") {
		t.Errorf("warnings %+v should include language_mismatch", got.Warnings)
	}
	if rec.opts[0].Language != "auto" || got.Language != "auto" {
		t.Errorf("told %q, reported %q; want auto for both", rec.opts[0].Language, got.Language)
	}
}

// A model with one language has nothing to be told, and keeps behaving as it
// always did: the request is echoed, a different language is warned about.
func TestASingleLanguageModelIsNotToldAnything(t *testing.T) {
	got, rec := transcribeIn(t, fakeRegistry{languages: []string{"ru"}}, "ru-RU")
	if rec.opts[0].Language != "" {
		t.Errorf("a monolingual model was told %q", rec.opts[0].Language)
	}
	if got.Language != "ru-RU" || hasWarning(got.Warnings, "language_mismatch") {
		t.Errorf("language %q, warnings %+v", got.Language, got.Warnings)
	}

	// "auto" is a request any model can honour, and a monolingual one reports
	// the language it has rather than echoing the word.
	got, _ = transcribeIn(t, fakeRegistry{languages: []string{"ru"}}, "auto")
	if got.Language != "ru" || hasWarning(got.Warnings, "language_mismatch") {
		t.Errorf("auto on a monolingual model: language %q, warnings %+v", got.Language, got.Warnings)
	}

	got, _ = transcribeIn(t, fakeRegistry{languages: []string{"ru"}}, "en")
	if !hasWarning(got.Warnings, "language_mismatch") {
		t.Errorf("warnings %+v should include language_mismatch", got.Warnings)
	}
}
