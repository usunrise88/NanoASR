package era

import "github.com/usunrise88/nanoasr/internal/api/adapter"

// Doc describes this dialect for /docs.
func (*Adapter) Doc() adapter.Doc {
	return adapter.Doc{
		Name:  "era",
		Title: "whisper-asr-webservice contract",
		Summary: "For clients written against ahmetoner/whisper-asr-webservice. It mounts " +
			"at the root, so it is enabled deliberately rather than by default.",
		Routes: []adapter.Route{{
			Method:  "POST",
			Path:    "/asr",
			Summary: "Transcribe and return the transcript as a file.",
			Detail: "multipart with audio_file, plus output (txt, json, srt, vtt, tsv), " +
				"task, language, word_timestamps and encode. task=translate is " +
				"refused rather than answered with a transcription.",
		}, {
			Method:  "POST",
			Path:    "/detect-language",
			Summary: "Report the language of the audio.",
			Detail: "Answered from the model's declared languages, not from an acoustic " +
				"language identifier: these are monolingual models, so the honest " +
				"answer is what the model was trained on.",
		}, {
			Method:  "POST",
			Path:    "/asr_task",
			Summary: "Queue the same work and return a task id.",
		}, {
			Method:  "GET",
			Path:    "/asr_task/{task_id}",
			Summary: "Poll a queued task.",
		}},
	}
}
