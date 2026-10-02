package openai

import "github.com/usunrise88/nanoasr/internal/api/adapter"

// Doc describes this dialect for /docs. The divergences listed here are the
// ones in the package comment: documented in the code and documented to the
// caller, because the second is the one that saves somebody an afternoon.
func (*Adapter) Doc() adapter.Doc {
	return adapter.Doc{
		Name:  "openai",
		Title: "OpenAI audio API",
		Summary: "Drop-in for /v1/audio/transcriptions, so an OpenAI SDK works by " +
			"changing base_url and nothing else.",
		Routes: []adapter.Route{{
			Method:  "POST",
			Path:    "/v1/audio/transcriptions",
			Summary: "Transcribe an uploaded file.",
			Detail: "multipart/form-data with file, and optionally model, language, " +
				"response_format (json, verbose_json, text, srt, vtt) and repeated " +
				"timestamp_granularities[] (word, segment). prompt is applied as a " +
				"comma-separated hotword list rather than as an LM prompt, and " +
				"temperature is accepted and ignored because the decoders do not " +
				"sample; both are reported back as warnings. Asking for word " +
				"timings from a model that cannot produce them yields segment " +
				"timings and a warning, or 422 with X-NanoASR-Strict: 1.",
		}, {
			Method:  "POST",
			Path:    "/v1/audio/translations",
			Summary: "Not implemented: this build ships transcription models only.",
			Detail:  "Answers 501. Translation needs a model that writes a language it did not hear.",
		}, {
			Method:  "GET",
			Path:    "/v1/models",
			Summary: "List the models that can be passed as `model`.",
			Detail: "Transcription models only: the supporting models (VAD, punctuation, " +
				"diarization) and the streaming models are left out, because passing " +
				"one here would fail. Each entry carries the NanoASR state, languages " +
				"and whether it produces word timestamps.",
		}, {
			Method:  "GET",
			Path:    "/v1/models/{id}",
			Summary: "Describe one model.",
		}},
	}
}
