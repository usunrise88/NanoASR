// Package native implements the NanoASR dialect: everything the server can do,
// without OpenAI's constraints. Errors use RFC 9457 problem+json.
package native

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	"github.com/usunrise88/nanoasr/internal/api/subtitle"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/httpx"
)

func init() { adapter.Register(&Adapter{}) }

// uploadMemory is how much of a multipart request stays in RAM before net/http
// spools it to disk. Small on purpose: audio belongs on disk, and a queued job
// needs it there anyway.
const uploadMemory = 1 << 20

// retryAfter is what a queue-pressure 429 advises. Long enough that a client
// backing off actually relieves the pressure, short enough to stay usable. A
// rate-limit 429 knows its own wait and says that instead.
const retryAfter = 5 * time.Second

type Adapter struct{}

func (*Adapter) Name() string { return "native" }

func (a *Adapter) Mount(mux *http.ServeMux, svc core.Service, deps adapter.Deps) {
	// Transcription: synchronous and queued.
	mux.HandleFunc("POST /api/v1/transcribe", a.transcribe(svc))
	mux.HandleFunc("POST /api/v1/jobs", a.submit(svc))
	mux.HandleFunc("GET /api/v1/jobs", a.listJobs(svc))
	mux.HandleFunc("GET /api/v1/jobs/{id}", a.job(svc))
	mux.HandleFunc("GET /api/v1/jobs/{id}/events", a.jobEvents(svc)) // SSE
	mux.HandleFunc("DELETE /api/v1/jobs/{id}", a.cancel(svc))

	// Models: reading is open to any key, changing state is not.
	mux.HandleFunc("GET /api/v1/models", a.models(deps))
	mux.HandleFunc("GET /api/v1/catalog", a.catalog(deps))

	// The refusal has to speak this dialect's error shape, not a generic one.
	admin := httpx.RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		WriteProblem(w, r, core.Errorf(core.CodeModelForbidden,
			"this API key is not permitted to administer models"))
	})
	for pattern, h := range map[string]http.HandlerFunc{
		"POST /api/v1/models/{id}/download": a.download(deps), // SSE progress
		"POST /api/v1/models/{id}/load":     a.load(deps),
		"POST /api/v1/models/{id}/unload":   a.unload(deps),
		"POST /api/v1/models/{id}/pin":      a.pin(deps),
		"POST /api/v1/models/{id}/reload":   a.reload(deps), // hot swap
		"GET /api/v1/config":                a.config(deps),
	} {
		mux.Handle(pattern, admin(h))
	}
}

// --- transcription ----------------------------------------------------------

func (*Adapter) transcribe(svc core.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		source, err := adapter.NewUploadSource(r, "file", uploadMemory)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		// Synchronous: the response carries the transcript, so the upload has
		// no reason to outlive the request.
		defer source.Close()

		p, err := parseParams(r, source)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}

		result, err := svc.Transcribe(r.Context(), p.request)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		render(w, result, p.format)
	}
}

func (*Adapter) submit(svc core.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		source, err := adapter.NewUploadSource(r, "file", uploadMemory)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		// No defer Close here: on the happy path the queue takes ownership of
		// the file, and closing would delete work that has already been
		// accepted. Every failure below closes explicitly instead.
		p, err := parseParams(r, source)
		if err != nil {
			source.Close()
			WriteProblem(w, r, err)
			return
		}

		job, err := svc.Submit(r.Context(), p.request)
		if err != nil {
			source.Close()
			WriteProblem(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, job)
	}
}

func (*Adapter) listJobs(svc core.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filter, err := parseFilter(r)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		page, err := svc.ListJobs(r.Context(), filter)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	}
}

func (*Adapter) job(svc core.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		job, err := svc.Job(r.Context(), r.PathValue("id"))
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		// A finished job can be asked for as subtitles, which is what makes
		// the queued path as useful as the synchronous one.
		if format := formValue(r, "response_format"); format != "" && format != formatJSON {
			if !validFormat(format) {
				WriteProblem(w, r, core.Errorf(core.CodeInvalidRequest,
					"response_format %q is not one of json, text, srt, vtt", format).
					WithParam("response_format"))
				return
			}
			if job.Result == nil {
				WriteProblem(w, r, core.Errorf(core.CodeInvalidRequest,
					"job %s is %s and has no transcript to render as %s",
					job.ID, job.Status, format).WithParam("response_format"))
				return
			}
			render(w, job.Result, format)
			return
		}
		writeJSON(w, http.StatusOK, job)
	}
}

func (*Adapter) cancel(svc core.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := svc.Cancel(r.Context(), id); err != nil {
			WriteProblem(w, r, err)
			return
		}
		// Read back rather than assume: a job that finished a moment before the
		// cancellation arrived is succeeded, not canceled, and saying otherwise
		// would be a lie the client acts on.
		job, err := svc.Job(r.Context(), id)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, job)
	}
}

func (*Adapter) jobEvents(svc core.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Subscribe before any streaming header is written: an unknown or
		// someone else's job has to answer 404, and after WriteHeader(200) the
		// only way left to report it is inside the stream.
		events, err := svc.Watch(r.Context(), r.PathValue("id"), httpx.LastEventID(r))
		if err != nil {
			WriteProblem(w, r, err)
			return
		}

		stream, err := httpx.NewStream(w)
		if err != nil {
			WriteProblem(w, r, core.Errorf(core.CodeInternal, "%s", err.Error()))
			return
		}
		defer stream.Close()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-stream.Heartbeat():
				if stream.Comment("keep-alive") != nil {
					return
				}
			case ev, ok := <-events:
				if !ok {
					// The job is over. Say so explicitly: an EventSource
					// reconnects when the server closes a stream, and a client
					// that does not know the work is finished would reconnect
					// forever.
					_ = stream.Send("", "done", map[string]string{"reason": "terminal"})
					return
				}
				// Seq 0 is a catch-up snapshot rather than a transition, and
				// carries no id: the client's Last-Event-ID should stay where
				// it was rather than advance past events that never existed.
				id := ""
				if ev.Seq > 0 {
					id = strconv.FormatInt(ev.Seq, 10)
				}
				if stream.Send(id, string(ev.Job.Status), ev.Job) != nil {
					return
				}
			}
		}
	}
}

// --- models -----------------------------------------------------------------

func (*Adapter) models(deps adapter.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Models == nil {
			WriteProblem(w, r, core.Errorf(core.CodeInternal, "model service is not configured"))
			return
		}
		infos, err := deps.Models.List(r.Context())
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": infos})
	}
}

func (*Adapter) catalog(deps adapter.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Models == nil {
			WriteProblem(w, r, core.Errorf(core.CodeInternal, "model service is not configured"))
			return
		}
		entries, err := deps.Models.Catalog(r.Context())
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": entries})
	}
}

func (*Adapter) download(deps adapter.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Models == nil {
			WriteProblem(w, r, core.Errorf(core.CodeInternal, "model service is not configured"))
			return
		}
		// Same reason as jobEvents: refusals — an unknown model, downloading
		// disabled, a licence the operator declined — must land as a status
		// code, not as a line inside a 200.
		progress, err := deps.Models.Download(r.Context(), r.PathValue("id"))
		if err != nil {
			WriteProblem(w, r, err)
			return
		}

		stream, err := httpx.NewStream(w)
		if err != nil {
			WriteProblem(w, r, core.Errorf(core.CodeInternal, "%s", err.Error()))
			return
		}
		defer stream.Close()

		var seq int64
		for {
			select {
			case <-r.Context().Done():
				return
			case <-stream.Heartbeat():
				if stream.Comment("keep-alive") != nil {
					return
				}
			case tick, ok := <-progress:
				if !ok {
					_ = stream.Send("", "done", map[string]string{"reason": "terminal"})
					return
				}
				seq++
				name := "progress"
				if tick.Done || tick.Err != "" {
					name = "done"
				}
				if stream.Send(strconv.FormatInt(seq, 10), name, tick) != nil {
					return
				}
			}
		}
	}
}

func (*Adapter) load(deps adapter.Deps) http.HandlerFunc {
	return modelAction(deps, func(r *http.Request, m core.ModelService, id string) error {
		return m.Load(r.Context(), id)
	})
}

func (*Adapter) unload(deps adapter.Deps) http.HandlerFunc {
	return modelAction(deps, func(r *http.Request, m core.ModelService, id string) error {
		return m.Unload(r.Context(), id)
	})
}

func (*Adapter) pin(deps adapter.Deps) http.HandlerFunc {
	return modelAction(deps, func(r *http.Request, m core.ModelService, id string) error {
		// Defaulting to true keeps the common call a bare POST; pinned=false
		// is how the same endpoint releases a model.
		pinned := true
		if v := formValue(r, "pinned"); v != "" {
			parsed, err := boolValue(r, "pinned")
			if err != nil {
				return err
			}
			pinned = parsed
		}
		return m.Pin(r.Context(), id, pinned)
	})
}

func (*Adapter) reload(deps adapter.Deps) http.HandlerFunc {
	return modelAction(deps, func(r *http.Request, m core.ModelService, id string) error {
		return m.Reload(r.Context(), id, formValue(r, "revision"))
	})
}

// modelAction is the shape every state-changing model endpoint shares: resolve
// the service, run the action, answer with the model's new state so a caller
// does not have to poll to find out what happened.
func modelAction(deps adapter.Deps, do func(*http.Request, core.ModelService, string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Models == nil {
			WriteProblem(w, r, core.Errorf(core.CodeInternal, "model service is not configured"))
			return
		}
		_ = r.ParseForm()

		id := r.PathValue("id")
		if err := do(r, deps.Models, id); err != nil {
			WriteProblem(w, r, err)
			return
		}

		infos, err := deps.Models.List(r.Context())
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		for _, info := range infos {
			if info.ID == id {
				writeJSON(w, http.StatusOK, info)
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id, "state": string(core.ModelAbsent)})
	}
}

func (*Adapter) config(deps adapter.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.ConfigSnapshot == nil {
			WriteProblem(w, r, core.Errorf(core.CodeNotImplemented,
				"this server does not expose its configuration"))
			return
		}
		writeJSON(w, http.StatusOK, deps.ConfigSnapshot())
	}
}

// --- filters and rendering --------------------------------------------------

func parseFilter(r *http.Request) (core.JobFilter, error) {
	q := r.URL.Query()
	f := core.JobFilter{
		ModelID: q.Get("model"),
		Cursor:  q.Get("cursor"),
	}

	for _, s := range q["status"] {
		switch st := core.JobStatus(s); st {
		case core.JobQueued, core.JobRunning, core.JobSucceeded,
			core.JobFailed, core.JobCanceled, core.JobExpired:
			f.Status = append(f.Status, st)
		default:
			return f, core.Errorf(core.CodeInvalidRequest,
				"status %q is not a job status", s).WithParam("status")
		}
	}

	if v := q.Get("source"); v != "" {
		switch src := core.Source(v); src {
		case core.SourceAPI, core.SourceUI:
			f.Source = src
		default:
			return f, core.Errorf(core.CodeInvalidRequest,
				"source %q is not one of api, ui", v).WithParam("source")
		}
	}

	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return f, core.Errorf(core.CodeInvalidRequest,
				"limit must be a positive integer").WithParam("limit")
		}
		f.Limit = n
	}

	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, core.Errorf(core.CodeInvalidRequest,
				"since must be an RFC 3339 timestamp").WithParam("since")
		}
		f.Since = &t
	}
	return f, nil
}

// render writes a result in the requested format. Unlike the OpenAI dialect,
// json here is the whole result: this dialect exists because that shape is what
// a NanoASR client actually wants.
func render(w http.ResponseWriter, res *core.Result, format string) {
	switch format {
	case formatText:
		writeText(w, "text/plain; charset=utf-8", res.Text+"\n")
	case formatSRT:
		writeText(w, subtitle.SRTContentType, subtitle.SRT(res))
	case formatVTT:
		writeText(w, subtitle.VTTContentType, subtitle.VTT(res))
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeText(w http.ResponseWriter, contentType, body string) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// problem is RFC 9457 application/problem+json.
type problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
	Code     string `json:"code"`
	Param    string `json:"param,omitempty"`
}

// WriteProblem renders a domain error. Exported so a custom dialect can reuse
// the same shape.
func WriteProblem(w http.ResponseWriter, r *http.Request, err error) {
	// A 429 without Retry-After tells a client to back off without saying how
	// far, which in practice means it retries immediately.
	if core.AsError(err).Code.HTTPStatus() == http.StatusTooManyRequests &&
		w.Header().Get("Retry-After") == "" {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
	}
	writeProblemWithoutRetryAfter(w, r, err)
}

func writeProblemWithoutRetryAfter(w http.ResponseWriter, r *http.Request, err error) {
	e := core.AsError(err)
	status := e.Code.HTTPStatus()

	p := problem{
		Type:     "https://docs.nanoasr.dev/errors/" + string(e.Code),
		Title:    http.StatusText(status),
		Status:   status,
		Detail:   e.Message,
		Instance: r.URL.Path,
		Code:     string(e.Code),
		Param:    e.Param,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

// DenyRateLimit renders a rate-limit refusal as problem+json, so a client's
// error handling recognises it as the same shape as every other refusal here.
//
// Exported because the middleware that produces it is mounted in main, outside
// this package, and the alternative — a 429 in a shape this dialect never
// otherwise emits — is a refusal clients parse by accident or not at all.
func DenyRateLimit(w http.ResponseWriter, r *http.Request, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(httpx.RetryAfterSeconds(wait)))
	writeProblemWithoutRetryAfter(w, r, core.Errorf(core.CodeRateLimited,
		"this API key is over its request rate; retry in %s", wait.Round(time.Millisecond)))
}
