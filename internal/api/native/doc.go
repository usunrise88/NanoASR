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
			Detail: "multipart/form-data with file, plus model, language (a locale of a " +
				"multilingual model, or auto), channel_mode, " +
				"diarize, num_speakers, punctuate, itn, hotwords, hotwords_dict, " +
				"hotwords_score, decoding_method and word_timestamps. hotwords_dict " +
				"names stored dictionaries by key and merges their phrases into " +
				"hotwords. Cancelling the request stops the decode between " +
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
			Method:  "PATCH",
			Path:    "/api/v1/jobs/{id}",
			Summary: "Name the speakers of a finished diarized job.",
			Detail: `JSON: {"speaker_names": {"spk_0": "Anna"}}; an empty name gives a ` +
				"speaker its id back. The names are kept with the result as " +
				"speakers[].name, beside the ids the segments carry, and label the " +
				"srt and vtt cues.",
		}, {
			Method:  "DELETE",
			Path:    "/api/v1/jobs/{id}",
			Summary: "Cancel a queued or running job.",
			Detail:  "A diarization pass already under way cannot be interrupted and runs to its end.",
		}, {
			Method:  "GET",
			Path:    "/api/v1/isolation",
			Summary: "Say whether voice isolation is on, its download formats, and whether isolations may be kept.",
			Detail: "{enabled, formats, keep}. Every /api/v1/isolations route answers " +
				"capability_unavailable on a server where it is off. The steps: upload " +
				"(POST /isolations), wait for awaiting_selection, listen to the voices' " +
				"samples, then keep one (extract), every one (extract-all) or transcribe " +
				"them all by speaker (POST transcript); fetch tracks and the transcript " +
				"once the isolation is done.",
		}, {
			Method:  "GET",
			Path:    "/api/v1/isolations",
			Summary: "List this key's isolations, the newest first.",
			Detail:  "{data: [isolation]}. An administrative key sees every one.",
		}, {
			Method:  "POST",
			Path:    "/api/v1/isolations",
			Summary: "Upload a recording to isolate voices from.",
			Detail: "multipart/form-data with file, num_speakers if known, and keep=true " +
				"to store it until it is deleted. Answers 202 with an isolation that " +
				"finds the voices (status analysing) and then waits for a choice " +
				"(awaiting_selection). Unlike a job, its audio is kept until " +
				"isolation.ttl passes with nothing happening, it is deleted, or the " +
				"server stops; a kept one survives all but the delete.",
		}, {
			Method:  "GET",
			Path:    "/api/v1/isolations/{id}",
			Summary: "Fetch an isolation: its voices, tracks and transcript progress.",
			Detail: "status is queued, analysing, awaiting_selection, extracting, " +
				"transcribing, done or failed. voices lists each speaker with its name, " +
				"speaking time and the 3-second sample it would be isolated by, or the " +
				"reason it has none; tracks the voices isolated so far; transcript the " +
				"state of the transcript by speakers and its jobs.",
		}, {
			Method:  "PATCH",
			Path:    "/api/v1/isolations/{id}",
			Summary: "Name the voices, and keep the isolation or let it expire.",
			Detail: `JSON: {"names": {"spk_0": "Anna"}, "keep": true}; either may be ` +
				"left out, and an empty name gives a voice its id back. Names label " +
				"the tracks' files and the transcript, including one already made.",
		}, {
			Method:  "GET",
			Path:    "/api/v1/isolations/{id}/events",
			Summary: "Follow an isolation as server-sent events.",
			Detail: "Numbered states until the isolation waits on a person or is done, " +
				"then a done event. Watch again after starting the next step.",
		}, {
			Method:  "GET",
			Path:    "/api/v1/isolations/{id}/voices/{speaker}/sample",
			Summary: "Listen to a voice: its sample as WAV.",
			Detail:  "Exactly the 3 seconds the extractor is told to keep.",
		}, {
			Method:  "POST",
			Path:    "/api/v1/isolations/{id}/extract",
			Summary: "Isolate one voice onto its track.",
			Detail: "JSON or form: speaker, and sample_start in seconds to take the " +
				"sample from somewhere else. May be repeated once nothing is running; " +
				"isolating a voice again replaces its own track and no other.",
		}, {
			Method:  "POST",
			Path:    "/api/v1/isolations/{id}/extract-all",
			Summary: "Isolate every voice, a track each.",
			Detail: "Every voice with a sample and no track yet. A voice without a " +
				"sample is skipped with a warning: isolate it with extract and sample_start.",
		}, {
			Method:  "GET",
			Path:    "/api/v1/isolations/{id}/tracks/{speaker}",
			Summary: "Download one voice's track.",
			Detail: "format=wav (default) or mp3, 16 kHz mono, as long as the recording. " +
				"WAV is served with Range, so a player starts at once and seeks anywhere. " +
				"MP3 needs ffmpeg on the server. /result?speaker= is the same; " +
				"/result alone is the voice chosen last.",
		}, {
			Method:  "GET",
			Path:    "/api/v1/isolations/{id}/media",
			Summary: "URLs a media element can play the recording, tracks and samples from.",
			Detail: "{audio, tracks: {speaker: url}, samples: {speaker: url}, expires_at}. An " +
				"<audio src> cannot send the API key, so on a server with keys each URL " +
				"carries a ticket for itself: that path, for reading, for this key, for " +
				"12 hours. The key never goes in a URL; a restart voids every ticket.",
		}, {
			Method:  "GET",
			Path:    "/api/v1/isolations/{id}/peaks",
			Summary: "The waveform of the recording, or of one track with speaker=.",
			Detail: "buckets=2000 by default: RMS of equal stretches, the loudest 1. For a " +
				"player that streams the audio rather than decoding all of it to draw one.",
		}, {
			Method:  "GET",
			Path:    "/api/v1/isolations/{id}/audio",
			Summary: "Download the recording as the isolation holds it.",
			Detail:  "format=wav or mp3, 16 kHz mono, on the same timeline as the tracks.",
		}, {
			Method:  "POST",
			Path:    "/api/v1/isolations/{id}/transcribe",
			Summary: "Queue one track for transcription.",
			Detail: "Takes the fields of /api/v1/jobs except file, plus speaker, and " +
				"answers 202 with the job. The job holds its own copy of the audio.",
		}, {
			Method:  "POST",
			Path:    "/api/v1/isolations/{id}/transcript",
			Summary: "Transcribe by speaker: every voice isolated and transcribed alone.",
			Detail: "Takes the fields of /api/v1/jobs except file; diarize is ignored. " +
				"Isolates every voice still without a track, queues a job per track and " +
				"merges them, each word labelled with its voice. Two voices speaking at " +
				"once are both transcribed. Answers 202 with the isolation; follow its " +
				"events through extracting and transcribing to done.",
		}, {
			Method:  "GET",
			Path:    "/api/v1/isolations/{id}/transcript",
			Summary: "Fetch the transcript by speaker.",
			Detail: "response_format=json (default: a result whose speakers carry their " +
				"names), text (a dialogue, name: words), srt or vtt (labelled by name).",
		}, {
			Method:  "DELETE",
			Path:    "/api/v1/isolations/{id}",
			Summary: "Delete an isolation and its audio now, kept or not.",
			Detail:  "Stops its work, and cancels the jobs of a transcript in progress.",
		}, {
			Method:  "GET",
			Path:    "/api/v1/models",
			Summary: "List installed models and what is resident.",
		}, {
			Method:  "GET",
			Path:    "/api/v1/catalog",
			Summary: "List the models available for download.",
			Detail: "Each with its state here (absent, downloading or downloaded) and " +
				"size_bytes, what downloading it costs.",
		}, {
			Method:  "POST",
			Path:    "/api/v1/models/{id}/download",
			Summary: "Download a catalog model, streaming progress as SSE.",
			Detail: "Closing the stream does not stop the download; DELETE does. " +
				"A second request for the same model joins the one running.",
			Admin: true,
		}, {
			Method:  "DELETE",
			Path:    "/api/v1/models/{id}/download",
			Summary: "Cancel a download in progress.",
			Detail:  "Everyone following it gets the cancellation as its last event; nothing of it stays on disk.",
			Admin:   true,
		}, {
			Method:  "DELETE",
			Path:    "/api/v1/models/{id}",
			Summary: "Remove a downloaded model from disk.",
			Detail: "409 model_in_use while it serves a request, downloads, or is held by the " +
				"server since it started (the VAD, the streaming model, the diarizer).",
			Admin: true,
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
			Path:    "/api/v1/hotwords",
			Summary: "List hotword dictionaries, with the server's biasing policy.",
			Detail: "q searches keys, names, descriptions and the phrases themselves; " +
				"limit caps the page. Phrases are left out of a listing — fetch one " +
				"dictionary for those — and policy says whether biasing is switched " +
				"on at all, which models it can apply to being a separate question " +
				"answered per model in /api/v1/models.",
		}, {
			Method:  "GET",
			Path:    "/api/v1/hotwords/{key}",
			Summary: "One dictionary and its phrases.",
			Detail:  "response_format=text returns the plain file the import endpoint reads back.",
		}, {
			Method:  "POST",
			Path:    "/api/v1/hotwords",
			Summary: "Create a dictionary.",
			Detail: "JSON: key, name, description, score, and either phrases as a list " +
				"or text as one block. Refuses a key that already exists; PUT replaces.",
			Admin: true,
		}, {
			Method:  "PUT",
			Path:    "/api/v1/hotwords/{key}",
			Summary: "Replace a dictionary wholesale.",
			Admin:   true,
		}, {
			Method:  "DELETE",
			Path:    "/api/v1/hotwords/{key}",
			Summary: "Delete a dictionary.",
			Admin:   true,
		}, {
			Method:  "POST",
			Path:    "/api/v1/hotwords/{key}/import",
			Summary: "Upload phrases into a dictionary, creating it if the key is new.",
			Detail: "multipart with a file field, or the file as the request body. " +
				"One phrase per line with # comments, a one-column CSV, a JSON array " +
				"of phrases, or a JSON dictionary as exported here. mode=append is " +
				"the default; mode=replace swaps the phrase list.",
			Admin: true,
		}, {
			Method:  "GET",
			Path:    "/api/v1/config",
			Summary: "The effective configuration, with secrets redacted.",
			Admin:   true,
		}},
	}
}
