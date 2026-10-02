package native

import "github.com/usunrise88/nanoasr/internal/api/adapter"

// Doc describes this dialect for /docs.
func (*Adapter) Doc() adapter.Doc {
	return adapter.Doc{
		Name:  "native",
		Title: "NanoASR API",
		Summary: "Everything the server can do, in its own shapes: queued jobs with " +
			"progress, word timings, diarization, and model administration. Errors " +
			"are problem+json (RFC 9457).",
		Routes: []adapter.Route{{
			Method:  "POST",
			Path:    "/api/v1/transcribe",
			Summary: "Transcribe an uploaded file and wait for the result.",
			Detail: "multipart/form-data with file, plus model, language, channel_mode, " +
				"diarize, num_speakers, punctuate, itn, hotwords, decoding_method and " +
				"word_timestamps. Cancelling the request stops the decode between " +
				"batches rather than at the end of the file.",
		}, {
			Method:  "POST",
			Path:    "/api/v1/jobs",
			Summary: "Queue the same work and return a job.",
			Detail: "Takes the same fields as transcribe, plus webhook_url for a signed " +
				"delivery on completion. The upload is held on disk until the job " +
				"reaches a terminal state and is then deleted.",
		}, {
			Method:  "GET",
			Path:    "/api/v1/jobs",
			Summary: "List this key's jobs.",
			Detail:  "Scoped to the calling key unless the key is administrative. Paged by cursor.",
		}, {
			Method:  "GET",
			Path:    "/api/v1/jobs/{id}",
			Summary: "Fetch one job and its result.",
		}, {
			Method:  "GET",
			Path:    "/api/v1/jobs/{id}/events",
			Summary: "Follow a job as server-sent events.",
			Detail: "text/event-stream of numbered job states. Last-Event-ID resumes " +
				"rather than replays. A job that has already finished yields one " +
				"catch-up event and closes.",
		}, {
			Method:  "DELETE",
			Path:    "/api/v1/jobs/{id}",
			Summary: "Cancel a queued or running job.",
			Detail:  "A diarization pass already under way cannot be interrupted and runs to its end.",
		}, {
			Method:  "GET",
			Path:    "/api/v1/models",
			Summary: "List installed models and what is resident.",
		}, {
			Method:  "GET",
			Path:    "/api/v1/catalog",
			Summary: "List the models available for download.",
		}, {
			Method:  "POST",
			Path:    "/api/v1/models/{id}/download",
			Summary: "Download a catalog model, streaming progress as SSE.",
			Admin:   true,
		}, {
			Method:  "POST",
			Path:    "/api/v1/models/{id}/load",
			Summary: "Load a model into memory now.",
			Admin:   true,
		}, {
			Method:  "POST",
			Path:    "/api/v1/models/{id}/unload",
			Summary: "Release a model.",
			Admin:   true,
		}, {
			Method:  "POST",
			Path:    "/api/v1/models/{id}/pin",
			Summary: "Keep a model resident, or stop keeping it.",
			Admin:   true,
		}, {
			Method:  "POST",
			Path:    "/api/v1/models/{id}/reload",
			Summary: "Swap in another revision without dropping requests.",
			Detail:  "The new instance is loaded and warmed before the pointer moves.",
			Admin:   true,
		}, {
			Method:  "GET",
			Path:    "/api/v1/config",
			Summary: "The effective configuration, with secrets redacted.",
			Admin:   true,
		}},
	}
}
