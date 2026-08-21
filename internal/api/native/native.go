// Package native implements the NanoASR dialect: everything the server can do,
// without OpenAI's constraints. Errors use RFC 9457 problem+json.
package native

import (
	"encoding/json"
	"net/http"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	"github.com/usunrise88/nanoasr/internal/core"
)

func init() { adapter.Register(&Adapter{}) }

type Adapter struct{}

func (*Adapter) Name() string { return "native" }

func (a *Adapter) Mount(mux *http.ServeMux, svc core.Service, deps adapter.Deps) {
	// Transcription: synchronous and queued.
	mux.HandleFunc("POST /api/v1/transcribe", todo())
	mux.HandleFunc("POST /api/v1/jobs", todo())
	mux.HandleFunc("GET /api/v1/jobs", todo())
	mux.HandleFunc("GET /api/v1/jobs/{id}", todo())
	mux.HandleFunc("GET /api/v1/jobs/{id}/events", todo()) // SSE
	mux.HandleFunc("DELETE /api/v1/jobs/{id}", todo())

	// Models: the admin surface the test UI drives.
	mux.HandleFunc("GET /api/v1/models", todo())
	mux.HandleFunc("GET /api/v1/catalog", todo())
	mux.HandleFunc("POST /api/v1/models/{id}/download", todo()) // SSE progress
	mux.HandleFunc("POST /api/v1/models/{id}/load", todo())
	mux.HandleFunc("POST /api/v1/models/{id}/unload", todo())
	mux.HandleFunc("POST /api/v1/models/{id}/pin", todo())
	mux.HandleFunc("POST /api/v1/models/{id}/reload", todo()) // hot swap

	mux.HandleFunc("GET /api/v1/config", todo())
}

// TODO(M3): implement the handlers above against core.Service and
// core.ModelService.
func todo() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { WriteProblem(w, r, core.ErrNotImplemented) }
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
