// Package openai implements the OpenAI audio API dialect.
//
// Deliberate divergences, each documented where it happens:
//   - /v1/audio/translations answers 501: this build ships no translation models.
//   - prompt maps to hotword biasing, not an LM prompt.
//   - temperature is accepted and ignored; the decoders do not sample.
//   - asking for word granularity from a model that cannot produce it yields a
//     warning and segment-level timings, because OpenAI SDK clients do not
//     expect a hard failure. X-NanoASR-Strict: 1 turns that into a 422.
package openai

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/httpx"
)

func init() { adapter.Register(&Adapter{}) }

// uploadMemory is how much of a multipart request stays in RAM before net/http
// spools it to disk. Small on purpose: audio belongs on disk, and the total is
// already capped by middleware.
const uploadMemory = 1 << 20

type Adapter struct{}

func (*Adapter) Name() string { return "openai" }

func (a *Adapter) Mount(mux *http.ServeMux, svc core.Service, deps adapter.Deps) {
	mux.HandleFunc("POST /v1/audio/transcriptions", a.transcriptions(svc, deps))
	mux.HandleFunc("POST /v1/audio/translations", a.translations())
	mux.HandleFunc("GET /v1/models", a.models(deps))
	mux.HandleFunc("GET /v1/models/{id}", a.model(deps))
}

func (*Adapter) transcriptions(svc core.Service, deps adapter.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		source, err := adapter.NewUploadSource(r, "file", uploadMemory)
		if err != nil {
			writeError(w, err)
			return
		}
		// The upload's spool is removed as soon as the response is written;
		// audio never outlives the request that carried it.
		defer source.Close()

		params, err := parseParams(r)
		if err != nil {
			writeError(w, err)
			return
		}

		req := params.toRequest(source)
		req.APIKeyID = httpx.APIKeyID(r.Context())

		result, err := svc.Transcribe(r.Context(), req)
		if err != nil {
			writeError(w, err)
			return
		}

		result.Warnings = append(result.Warnings, params.warnings()...)
		render(w, result, params.format, params.wantWords)
		_ = deps
	}
}

func (*Adapter) translations() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, core.Errorf(core.CodeNotImplemented,
			"translation is not supported: this build ships transcription models only"))
	}
}

// modelObject is OpenAI's model shape. Clients parse it strictly, so the extra
// NanoASR fields are additive rather than replacing anything.
type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`

	State          core.ModelState `json:"state"`
	Languages      []string        `json:"languages,omitempty"`
	WordTimestamps bool            `json:"word_timestamps"`
	License        string          `json:"license,omitempty"`
}

type modelList struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

func (*Adapter) models(deps adapter.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		infos, err := listModels(r.Context(), deps)
		if err != nil {
			writeError(w, err)
			return
		}
		out := modelList{Object: "list", Data: make([]modelObject, 0, len(infos))}
		for _, info := range infos {
			out.Data = append(out.Data, toModelObject(info))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func (*Adapter) model(deps adapter.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		infos, err := listModels(r.Context(), deps)
		if err != nil {
			writeError(w, err)
			return
		}
		for _, info := range infos {
			if info.ID == id {
				writeJSON(w, http.StatusOK, toModelObject(info))
				return
			}
		}
		writeError(w, core.Errorf(core.CodeModelNotFound, "no such model: %s", id).WithParam("model"))
	}
}

func listModels(ctx context.Context, deps adapter.Deps) ([]core.ModelInfo, error) {
	if deps.Models == nil {
		return nil, core.Errorf(core.CodeInternal, "model service is not configured")
	}
	return deps.Models.List(ctx)
}

func toModelObject(info core.ModelInfo) modelObject {
	return modelObject{
		ID:             info.ID,
		Object:         "model",
		Created:        0,
		OwnedBy:        "nanoasr",
		State:          info.State,
		Languages:      info.Languages,
		WordTimestamps: info.Capabilities.WordTimestamps,
		License:        info.License,
	}
}

// params is the request after validation, with the divergences resolved.
type params struct {
	model     string
	language  string
	prompt    string
	format    string
	wantWords bool
	strict    bool

	sawTemperature bool
}

func parseParams(r *http.Request) (params, error) {
	p := params{
		model:    strings.TrimSpace(r.FormValue("model")),
		language: strings.TrimSpace(r.FormValue("language")),
		prompt:   strings.TrimSpace(r.FormValue("prompt")),
		format:   strings.TrimSpace(r.FormValue("response_format")),
		strict:   isTruthy(r.Header.Get("X-NanoASR-Strict")),
	}

	if p.format == "" {
		p.format = formatJSON
	}
	if !validFormat(p.format) {
		return p, core.Errorf(core.CodeInvalidRequest,
			"response_format %q is not one of json, verbose_json, text, srt, vtt", p.format).
			WithParam("response_format")
	}

	if v := r.FormValue("temperature"); v != "" {
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			return p, core.Errorf(core.CodeInvalidRequest,
				"temperature must be a number").WithParam("temperature")
		}
		p.sawTemperature = true
	}

	// timestamp_granularities[] is a repeated field. Word granularity only
	// changes the response shape; whether real word timings exist is decided
	// by the model and reported as timestamp_source.
	if r.MultipartForm != nil {
		for _, key := range []string{"timestamp_granularities[]", "timestamp_granularities"} {
			for _, g := range r.MultipartForm.Value[key] {
				switch strings.TrimSpace(g) {
				case "word":
					p.wantWords = true
				case "segment":
				default:
					return p, core.Errorf(core.CodeInvalidRequest,
						"timestamp_granularities must be word or segment, got %q", g).
						WithParam("timestamp_granularities")
				}
			}
		}
	}
	// verbose_json without an explicit granularity still carries words: they
	// are the reason this server exists, and omitting them surprises callers.
	if p.format == formatVerboseJSON && !p.wantWords && !hasGranularity(r) {
		p.wantWords = true
	}
	return p, nil
}

func hasGranularity(r *http.Request) bool {
	if r.MultipartForm == nil {
		return false
	}
	return len(r.MultipartForm.Value["timestamp_granularities[]"])+
		len(r.MultipartForm.Value["timestamp_granularities"]) > 0
}

func (p params) toRequest(source core.AudioSource) core.Request {
	req := core.Request{
		Audio:    source,
		ModelID:  p.model,
		Language: p.language,
		Strict:   p.strict,
		Source:   core.SourceAPI,
	}
	// OpenAI's prompt biases an LM. The nearest honest equivalent here is
	// hotword biasing, and the difference is reported as a warning.
	if p.prompt != "" {
		for _, word := range strings.Split(p.prompt, ",") {
			if w := strings.TrimSpace(word); w != "" {
				req.Hotwords = append(req.Hotwords, w)
			}
		}
	}
	return req
}

func (p params) warnings() []core.Warning {
	var out []core.Warning
	if p.sawTemperature {
		out = append(out, core.Warning{
			Code:    "temperature_ignored",
			Message: "temperature has no effect: greedy and beam decoding do not sample",
		})
	}
	if p.prompt != "" {
		out = append(out, core.Warning{
			Code:    "prompt_mapped_to_hotwords",
			Message: "prompt was interpreted as a comma-separated hotword list, not as an LM prompt",
		})
	}
	return out
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func ln(v float64) float64 { return math.Log(v) }
