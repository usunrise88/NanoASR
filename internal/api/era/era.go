// Package era implements the whisper-asr-webservice dialect, Era extensions
// included, so that a client written against that service can point at NanoASR
// without changing a line.
//
// The contract is reproduced as it is published: /asr and /detect-language from
// the upstream project, /asr_task and /asr_task/{task_id} from the Era fork.
// Paths, parameter names, response envelopes, the Asr-Engine and
// Content-Disposition headers and the text/plain body of every output format are
// all as found there.
//
// Deliberate divergences, each documented where it happens:
//   - GET / does not redirect to /docs. There is no Swagger UI to redirect to,
//     and a dialect claiming the root would answer for every path no other
//     route matched — for the other dialects too.
//   - task=translate answers 501: this build ships no translation models.
//   - initial_prompt maps to hotword biasing, not an LM prompt.
//   - encode and vad_filter are accepted and inert; they are server settings
//     here, not request options.
//   - min_speakers/max_speakers collapse onto one exact count, because the
//     clusterer takes a count or a threshold and has nothing to do with a range.
//   - a task id is this server's job id with the requested output appended, so
//     that polling returns the format the submission asked for without keeping
//     a table of it in memory that a restart would lose.
//   - every endpoint is authenticated like the rest of the server; upstream has
//     no authentication at all.
//
// Warnings have no field in this contract, so they travel in the
// X-NanoASR-Warnings header rather than not travelling at all.
package era

import (
	"context"
	"net/http"
	"strings"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/httpx"
)

func init() { adapter.Register(&Adapter{}) }

// uploadMemory is how much of a multipart request stays in RAM before net/http
// spools it to disk, matching the other dialects.
const uploadMemory = 1 << 20

// uploadField is what this contract calls the file part. It is audio_file
// rather than file, and that difference is the whole reason a client cannot
// simply point at the OpenAI dialect instead.
const uploadField = "audio_file"

// taskIDSeparator joins a job id to the output format the submission asked for.
//
// A task id is opaque to the client — upstream's is a uuid4 — so carrying the
// format inside it costs nothing and replaces the in-memory table upstream
// keeps. Ours outlives the process, which is exactly when such a table would
// have been lost: jobs here survive a restart, so the answer to a poll must
// too. The separator cannot occur in a job id (job_<hex>) or in a format name.
const taskIDSeparator = "~"

type Adapter struct{}

func (*Adapter) Name() string { return "era" }

func (a *Adapter) Mount(mux *http.ServeMux, svc core.Service, deps adapter.Deps) {
	mux.HandleFunc("POST /asr", a.asr(svc))
	mux.HandleFunc("POST /detect-language", a.detectLanguage(svc, deps))
	mux.HandleFunc("POST /asr_task", a.asrTask(svc))
	mux.HandleFunc("GET /asr_task/{task_id}", a.asrTaskByID(svc))
}

// asr is the synchronous path: upload, wait, receive the transcript as a file.
func (*Adapter) asr(svc core.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		source, err := adapter.NewUploadSource(r, uploadField, uploadMemory)
		if err != nil {
			writeError(w, err)
			return
		}
		// The response carries the transcript, so the upload has no reason to
		// outlive the request.
		defer source.Close()

		p, err := parseParams(r, source)
		if err != nil {
			writeError(w, err)
			return
		}
		p.request.APIKeyID = httpx.APIKeyID(r.Context())

		result, err := svc.Transcribe(r.Context(), p.request)
		if err != nil {
			writeError(w, err)
			return
		}
		result.Warnings = append(result.Warnings, p.warn...)
		writeTranscript(w, result, p.output, source.Filename())
	}
}

// languageResponse is upstream's shape: the English name, the code, and how
// sure the answer is.
type languageResponse struct {
	DetectedLanguage string  `json:"detected_language"`
	LanguageCode     string  `json:"language_code"`
	Confidence       float64 `json:"confidence"`
}

// detectLanguage answers which language the transcript came back in.
//
// This build has no language identification model, and it does not pretend to
// have one. What it has is a recognition model that declares the languages it
// was trained on, and a request that resolves to exactly one such model — so
// the answer is that model's language, and the confidence says how much of a
// choice there was: 1 when the model can produce nothing else, 0 when it lists
// several and this endpoint had to take the first.
//
// It costs a full transcription, where upstream costs one encoder pass over
// thirty seconds. Recognising the file is the only way to learn which model the
// request actually resolved to without reaching past core.Service, and a
// dialect that reaches past it is a dialect that has grown a second copy of the
// model-selection rules.
func (*Adapter) detectLanguage(svc core.Service, deps adapter.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		source, err := adapter.NewUploadSource(r, uploadField, uploadMemory)
		if err != nil {
			writeError(w, err)
			return
		}
		defer source.Close()

		req := core.Request{Audio: source, Source: core.SourceAPI}
		req.APIKeyID = httpx.APIKeyID(r.Context())

		result, err := svc.Transcribe(r.Context(), req)
		if err != nil {
			writeError(w, err)
			return
		}
		if result.Language == "" {
			writeError(w, core.Errorf(core.CodeCapabilityUnavailable,
				"the recognition model declares no language, so there is none to report"))
			return
		}

		confidence := languageConfidence(r.Context(), deps, result.Model)
		if confidence == 0 {
			setWarnings(w, []core.Warning{{
				Code: "language_detection_unavailable",
				Message: "this build has no language identification model; the answer is " +
					"the first language the recognition model declares",
			}})
		}
		writeJSON(w, http.StatusOK, languageResponse{
			DetectedLanguage: languageName(result.Language),
			LanguageCode:     result.Language,
			Confidence:       confidence,
		})
	}
}

// languageConfidence is 1 when the model that produced the transcript declares
// exactly one language, and 0 otherwise. There is no middle value to report
// honestly: nothing here measured anything.
func languageConfidence(ctx context.Context, deps adapter.Deps, model string) float64 {
	if deps.Models == nil {
		return 0
	}
	infos, err := deps.Models.List(ctx)
	if err != nil {
		return 0
	}
	for _, info := range infos {
		if info.ID != model && info.ID+"@"+info.Revision != model {
			continue
		}
		if len(info.Languages) == 1 {
			return 1
		}
		return 0
	}
	return 0
}

// asrTask is the Era extension: submit and receive a handle.
func (*Adapter) asrTask(svc core.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		source, err := adapter.NewUploadSource(r, uploadField, uploadMemory)
		if err != nil {
			writeError(w, err)
			return
		}
		// No defer Close on the happy path: the queue takes ownership of the
		// file, and closing would delete work that has been accepted. Every
		// failure below closes explicitly.
		p, err := parseParams(r, source)
		if err != nil {
			source.Close()
			writeError(w, err)
			return
		}

		job, err := svc.Submit(r.Context(), p.request)
		if err != nil {
			source.Close()
			writeError(w, err)
			return
		}

		setWarnings(w, p.warn)
		writeJSON(w, http.StatusOK, taskAccepted{
			Success: true,
			TaskID:  job.ID + taskIDSeparator + p.output,
		})
	}
}

// asrTaskByID polls one task.
//
// The three answers are upstream's: not ready, ready and failed, and — for a
// task that finished — the transcript itself, with the same headers /asr would
// have sent. An unknown task is a 404, which is also what a task belonging to
// another API key looks like: whose job it is, is not something a caller should
// be able to probe for.
func (*Adapter) asrTaskByID(svc core.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, output := splitTaskID(r.PathValue("task_id"))
		// An explicit output wins, so a client that stored only the job id can
		// still ask for the format it wants.
		if v := value(r, "output"); v != "" {
			if !validOutput(v) {
				writeError(w, invalid("output",
					"output %q is not one of txt, vtt, srt, tsv, json", v))
				return
			}
			output = v
		}

		job, err := svc.Job(r.Context(), id)
		if err != nil {
			writeError(w, notFoundAsTask(err))
			return
		}

		switch job.Status {
		case core.JobQueued, core.JobRunning:
			writeJSON(w, http.StatusOK, taskState{Success: true, IsReady: false})
		case core.JobSucceeded:
			if job.Result == nil {
				// Succeeded without a transcript would be a bug in the store,
				// not a state a client should have to model.
				writeJSON(w, http.StatusOK, taskState{
					Success: false,
					IsReady: true,
					Error: &taskError{
						Title:     string(core.CodeInternal),
						Message:   "the task succeeded but its transcript is missing",
						Traceback: []string{"code: " + string(core.CodeInternal)},
					},
				})
				return
			}
			writeTranscript(w, job.Result, output, job.Filename)
		default:
			writeJSON(w, http.StatusOK, taskState{
				Success: false,
				IsReady: true,
				Error:   errorOf(job),
			})
		}
	}
}

// splitTaskID takes the output format back off a task id. A bare job id is
// accepted too and falls back to the default output.
func splitTaskID(taskID string) (id, output string) {
	id, format, found := strings.Cut(taskID, taskIDSeparator)
	if found && validOutput(format) {
		return id, format
	}
	return taskID, defaultOutput
}

// notFoundAsTask restates "no such job" in this contract's words. Everything
// else — a server with no queue, a store that failed — keeps its own code.
func notFoundAsTask(err error) error {
	if e := core.AsError(err); e.Code == core.CodeJobNotFound {
		return core.Errorf(core.CodeJobNotFound, "task not found")
	}
	return err
}
