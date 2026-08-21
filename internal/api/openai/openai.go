// Package openai implements the OpenAI audio API dialect.
//
// Compatibility notes that are deliberate divergences, documented in SPEC §8.1:
//   - /v1/audio/translations answers 501: v1 ships no translation models.
//   - prompt maps to hotword biasing, not an LM prompt.
//   - temperature is accepted and ignored; the decoders do not sample.
//   - asking for word granularity from a model that cannot produce it yields a
//     warning and segment-level timings, because OpenAI SDK clients do not
//     expect a hard failure. X-NanoASR-Strict: 1 turns it into 422.
package openai

import (
	"net/http"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	"github.com/usunrise88/nanoasr/internal/core"
)

func init() { adapter.Register(&Adapter{}) }

type Adapter struct{}

func (*Adapter) Name() string { return "openai" }

func (a *Adapter) Mount(mux *http.ServeMux, svc core.Service, deps adapter.Deps) {
	mux.HandleFunc("POST /v1/audio/transcriptions", a.transcriptions(svc, deps))
	mux.HandleFunc("POST /v1/audio/translations", a.translations())
	mux.HandleFunc("GET /v1/models", a.models(deps))
	mux.HandleFunc("GET /v1/models/{id}", a.model(deps))
}

// TODO(M1): parse multipart, map fields onto core.Request, run svc.Transcribe,
// render json | verbose_json | text | srt | vtt.
func (*Adapter) transcriptions(core.Service, adapter.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeError(w, core.ErrNotImplemented)
	}
}

func (*Adapter) translations() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeError(w, core.Errorf(core.CodeNotImplemented,
			"translation is not supported: NanoASR v1 ships transcription models only"))
	}
}

// TODO(M2): project core.ModelInfo into the OpenAI list shape.
func (*Adapter) models(adapter.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { writeError(w, core.ErrNotImplemented) }
}

func (*Adapter) model(adapter.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { writeError(w, core.ErrNotImplemented) }
}
