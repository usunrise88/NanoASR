package native

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Response formats. The subtitle formats only make sense for a finished result,
// which is why an asynchronous submission accepts them but the format is applied
// when the result is fetched.
const (
	formatJSON    = "json"
	formatText    = "text"
	formatSRT     = "srt"
	formatVTT     = "vtt"
	defaultFormat = formatJSON
)

// params is one request to /transcribe or /jobs after validation.
//
// Both endpoints take the same thing — the difference is only whether the caller
// waits — so they share a parser. Anything a client mistypes is rejected by name
// rather than ignored: silently dropping "punctate" would leave the caller
// convinced they asked for punctuation.
type params struct {
	request core.Request
	format  string
}

func parseParams(r *http.Request, source core.AudioSource) (params, error) {
	p := params{
		format: defaultFormat,
		request: core.Request{
			Audio:  source,
			Source: core.SourceAPI,
		},
	}

	if v := formValue(r, "response_format"); v != "" {
		if !validFormat(v) {
			return p, core.Errorf(core.CodeInvalidRequest,
				"response_format %q is not one of json, text, srt, vtt", v).
				WithParam("response_format")
		}
		p.format = v
	}

	p.request.ModelID = formValue(r, "model")
	p.request.Language = formValue(r, "language")
	p.request.WebhookURL = formValue(r, "webhook_url")

	if v := formValue(r, "channel_mode"); v != "" {
		mode := core.ChannelMode(v)
		switch mode {
		case core.ChannelDownmix, core.ChannelFirst, core.ChannelSplit:
			p.request.ChannelMode = mode
		default:
			return p, core.Errorf(core.CodeInvalidRequest,
				"channel_mode %q is not one of downmix, first, split", v).
				WithParam("channel_mode")
		}
	}

	if v := formValue(r, "decoding_method"); v != "" {
		if v != "greedy_search" && v != "modified_beam_search" {
			return p, core.Errorf(core.CodeInvalidRequest,
				"decoding_method %q is not one of greedy_search, modified_beam_search", v).
				WithParam("decoding_method")
		}
		p.request.DecodingMethod = v
	}

	for _, f := range []struct {
		name string
		into *bool
	}{
		{"diarize", &p.request.Diarize},
		{"punctuate", &p.request.Punctuate},
		{"itn", &p.request.ITN},
		{"strict", &p.request.Strict},
	} {
		v, err := boolValue(r, f.name)
		if err != nil {
			return p, err
		}
		*f.into = v
	}
	// The header form exists so a client can turn on strict mode without
	// rewriting the body it already builds, matching the OpenAI dialect.
	if isTruthy(r.Header.Get("X-NanoASR-Strict")) {
		p.request.Strict = true
	}

	for _, f := range []struct {
		name string
		into *int
	}{
		{"num_speakers", &p.request.NumSpeakers},
		{"max_active_paths", &p.request.MaxActivePaths},
	} {
		v, err := intValue(r, f.name)
		if err != nil {
			return p, err
		}
		*f.into = v
	}

	if v := formValue(r, "hotwords_score"); v != "" {
		score, err := strconv.ParseFloat(v, 32)
		if err != nil {
			return p, core.Errorf(core.CodeInvalidRequest,
				"hotwords_score must be a number").WithParam("hotwords_score")
		}
		p.request.HotwordsScore = float32(score)
	}
	p.request.Hotwords = hotwords(r)

	return p, nil
}

// hotwords accepts the repeated form as well as one comma-separated field,
// because both are natural to write and neither is wrong.
func hotwords(r *http.Request) []string {
	var raw []string
	if r.MultipartForm != nil {
		raw = append(raw, r.MultipartForm.Value["hotwords[]"]...)
		raw = append(raw, r.MultipartForm.Value["hotwords"]...)
	} else {
		raw = append(raw, r.Form["hotwords[]"]...)
		raw = append(raw, r.Form["hotwords"]...)
	}

	var out []string
	for _, entry := range raw {
		for _, word := range strings.Split(entry, ",") {
			if w := strings.TrimSpace(word); w != "" {
				out = append(out, w)
			}
		}
	}
	return out
}

func validFormat(f string) bool {
	switch f {
	case formatJSON, formatText, formatSRT, formatVTT:
		return true
	}
	return false
}

// formValue reads a field from wherever this request carries it. Multipart for
// an upload, urlencoded or query for everything else.
func formValue(r *http.Request, name string) string {
	if r.MultipartForm != nil {
		if vs := r.MultipartForm.Value[name]; len(vs) > 0 {
			return strings.TrimSpace(vs[0])
		}
		return ""
	}
	return strings.TrimSpace(r.FormValue(name))
}

func boolValue(r *http.Request, name string) (bool, error) {
	v := formValue(r, name)
	if v == "" {
		return false, nil
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, core.Errorf(core.CodeInvalidRequest,
		"%s must be true or false, got %q", name, v).WithParam(name)
}

func intValue(r *http.Request, name string) (int, error) {
	v := formValue(r, name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, core.Errorf(core.CodeInvalidRequest,
			"%s must be a non-negative integer, got %q", name, v).WithParam(name)
	}
	return n, nil
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
