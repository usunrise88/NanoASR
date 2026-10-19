package native

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/httpx"
)

// mountIsolation serves voice isolation: upload, listen to the voices, pick
// one or take them all, fetch them or transcribe them — one track, or every
// track into one transcript by speaker.
//
// On a server without it every route answers capability_unavailable naming
// the setting, rather than 404: a client that finds the endpoint missing
// cannot tell a typo from a feature somebody switched off.
func (a *Adapter) mountIsolation(mux *http.ServeMux, deps adapter.Deps) {
	iso := deps.Isolation
	mux.HandleFunc("GET /api/v1/isolation", isolationInfo(iso))
	for pattern, h := range map[string]func(core.IsolationService) http.HandlerFunc{
		"GET /api/v1/isolations":                              listIsolations,
		"POST /api/v1/isolations":                             createIsolation,
		"GET /api/v1/isolations/{id}":                         getIsolation,
		"PATCH /api/v1/isolations/{id}":                       updateIsolation,
		"GET /api/v1/isolations/{id}/events":                  isolationEvents, // SSE
		"GET /api/v1/isolations/{id}/voices/{speaker}/sample": voiceSample,
		"POST /api/v1/isolations/{id}/extract":                extractVoice,
		"POST /api/v1/isolations/{id}/extract-all":            extractAll,
		"GET /api/v1/isolations/{id}/audio":                   isolationAudio,
		"GET /api/v1/isolations/{id}/result":                  isolationResult,
		"GET /api/v1/isolations/{id}/tracks/{speaker}":        isolationResult,
		"POST /api/v1/isolations/{id}/transcribe":             transcribeIsolation,
		"POST /api/v1/isolations/{id}/transcript":             transcribeSpeakers,
		"GET /api/v1/isolations/{id}/transcript":              speakerTranscript,
		"DELETE /api/v1/isolations/{id}":                      deleteIsolation,
	} {
		if iso == nil {
			mux.HandleFunc(pattern, isolationOff)
			continue
		}
		mux.HandleFunc(pattern, h(iso))
	}
}

func isolationOff(w http.ResponseWriter, r *http.Request) {
	WriteProblem(w, r, core.Errorf(core.CodeCapabilityUnavailable,
		"voice isolation is not enabled on this server; set isolation.enabled (it needs diarization.enabled too)"))
}

// isolationInfo tells a client, the UI first of all, whether to offer the
// feature at all and in which formats.
func isolationInfo(iso core.IsolationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if iso == nil {
			writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "formats": iso.Formats(), "keep": iso.CanKeep()})
	}
}

func createIsolation(iso core.IsolationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		source, err := adapter.NewUploadSource(r, "file", uploadMemory)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		// The service copies the upload before it answers, so this request's
		// spool can go as soon as it returns.
		defer func() { _ = source.Close() }()
		n, err := intValue(r, "num_speakers")
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		keep, err := boolValue(r, "keep")
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		out, err := iso.Create(r.Context(), core.IsolationRequest{Audio: source, NumSpeakers: n, Keep: keep,
			Source: core.SourceAPI})
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, out)
	}
}

func listIsolations(iso core.IsolationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := iso.List(r.Context())
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": out})
	}
}

func getIsolation(iso core.IsolationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := iso.Isolation(r.Context(), r.PathValue("id"))
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func isolationEvents(iso core.IsolationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// As for jobs: subscribe first, so a missing isolation is a 404 and
		// not a line inside a 200.
		events, err := iso.Watch(r.Context(), r.PathValue("id"), httpx.LastEventID(r))
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
					// Nothing more will happen until somebody acts: the voices
					// wait to be chosen, or the voice to be fetched.
					_ = stream.Send("", "done", map[string]string{"reason": "idle"})
					return
				}
				if stream.Send(strconv.FormatInt(ev.Seq, 10), string(ev.Isolation.Status), ev.Isolation) != nil {
					return
				}
			}
		}
	}
}

func voiceSample(iso core.IsolationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rc, err := iso.Sample(r.Context(), r.PathValue("id"), r.PathValue("speaker"))
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		defer rc.Close()
		w.Header().Set("Cache-Control", "private, max-age=300")
		serveAudio(w, rc, core.AudioWAV, "")
	}
}

// updateIsolation names voices and keeps or releases the isolation, from a
// JSON body: {"names": {"spk_0": "Anna"}, "keep": true}.
func updateIsolation(iso core.IsolationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var upd core.IsolationUpdate
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<16))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&upd); err != nil {
			WriteProblem(w, r, core.Errorf(core.CodeInvalidRequest,
				"the body is not an isolation update: %s", err.Error()))
			return
		}
		out, err := iso.Update(r.Context(), r.PathValue("id"), upd)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func extractAll(iso core.IsolationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := iso.ExtractAll(r.Context(), r.PathValue("id"))
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, out)
	}
}

func extractVoice(iso core.IsolationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sel, err := parseSelection(r)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		out, err := iso.Extract(r.Context(), r.PathValue("id"), sel)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, out)
	}
}

// parseSelection takes the choice as JSON or as a form, whichever the client
// finds natural; a form is what curl -F writes.
func parseSelection(r *http.Request) (core.IsolationSelection, error) {
	var sel core.IsolationSelection
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/json" {
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<16))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&sel); err != nil {
			return sel, core.Errorf(core.CodeInvalidRequest, "the body is not a voice selection: %s", err.Error())
		}
	} else {
		if err := parseForm(r); err != nil {
			return sel, err
		}
		sel.Speaker = formValue(r, "speaker")
		if v := formValue(r, "sample_start"); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return sel, core.Errorf(core.CodeInvalidRequest, "sample_start must be a number of seconds").
					WithParam("sample_start")
			}
			sel.SampleStart = &f
		}
	}
	if strings.TrimSpace(sel.Speaker) == "" {
		return sel, core.Errorf(core.CodeInvalidRequest, "speaker is required: one of the voices found").
			WithParam("speaker")
	}
	return sel, nil
}

// parseForm reads a urlencoded or multipart body; a request with neither has
// its fields, if any, in the query.
func parseForm(r *http.Request) error {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var err error
	if ct == "multipart/form-data" {
		err = r.ParseMultipartForm(uploadMemory)
	} else {
		err = r.ParseForm()
	}
	if err != nil {
		return core.Errorf(core.CodeInvalidRequest, "cannot read the form: %s", err.Error())
	}
	return nil
}

// audioFormat reads ?format=, WAV when absent.
func audioFormat(r *http.Request) core.AudioFormat {
	format := core.AudioFormat(strings.ToLower(r.URL.Query().Get("format")))
	if format == "" {
		return core.AudioWAV
	}
	return format
}

func isolationAudio(iso core.IsolationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, format := r.PathValue("id"), audioFormat(r)
		rc, err := iso.Audio(r.Context(), id, format)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		defer rc.Close()
		name := "audio"
		if info, err := iso.Isolation(r.Context(), id); err == nil && info.Filename != "" {
			name = strings.TrimSuffix(filepath.Base(info.Filename), filepath.Ext(info.Filename))
		}
		serveAudio(w, rc, format, name+"."+string(format))
	}
}

// isolationResult serves a track: the one named in the path or by ?speaker=,
// else the voice chosen last.
func isolationResult(iso core.IsolationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, format := r.PathValue("id"), audioFormat(r)
		speaker := r.PathValue("speaker")
		if speaker == "" {
			speaker = r.URL.Query().Get("speaker")
		}
		rc, err := iso.Result(r.Context(), id, speaker, format)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		defer rc.Close()
		name := "isolated"
		if info, err := iso.Isolation(r.Context(), id); err == nil {
			switch {
			case speaker == "" && info.Result != nil:
				name = info.Result.Name
			case speaker == "" && len(info.Tracks) == 1:
				name = info.Tracks[0].Name
			}
			for _, t := range info.Tracks {
				if t.Speaker == speaker {
					name = t.Name
				}
			}
		}
		serveAudio(w, rc, format, name+"."+string(format))
	}
}

// serveAudio streams an audio body. A WAV knows its length; an MP3 is encoded
// as it is sent and does not.
func serveAudio(w http.ResponseWriter, rc io.Reader, format core.AudioFormat, filename string) {
	w.Header().Set("Content-Type", format.ContentType())
	if s, ok := rc.(interface{ Size() int64 }); ok {
		w.Header().Set("Content-Length", strconv.FormatInt(s.Size(), 10))
	}
	if filename != "" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	}
	w.WriteHeader(http.StatusOK)
	// Past the header there is no status left to report a failure with; a
	// truncated body is what the client sees, and the log has the reason.
	_, _ = io.Copy(w, rc)
}

func transcribeIsolation(iso core.IsolationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := parseForm(r); err != nil {
			WriteProblem(w, r, err)
			return
		}
		p, err := parseParams(r, nil)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		job, err := iso.Transcribe(r.Context(), r.PathValue("id"), formValue(r, "speaker"), p.request)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, job)
	}
}

// transcribeSpeakers starts the transcript by speakers, with the parameters
// a job takes; the answer is the isolation, which carries its progress.
func transcribeSpeakers(iso core.IsolationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := parseForm(r); err != nil {
			WriteProblem(w, r, err)
			return
		}
		p, err := parseParams(r, nil)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		out, err := iso.TranscribeSpeakers(r.Context(), r.PathValue("id"), p.request)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, out)
	}
}

func speakerTranscript(iso core.IsolationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		format := r.URL.Query().Get("response_format")
		if format == "" {
			format = formatJSON
		}
		if !validFormat(format) {
			WriteProblem(w, r, core.Errorf(core.CodeInvalidRequest,
				"response_format %q is not one of json, text, srt, vtt", format).WithParam("response_format"))
			return
		}
		res, err := iso.Transcript(r.Context(), r.PathValue("id"))
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		switch format {
		case formatJSON:
			writeJSON(w, http.StatusOK, res)
		case formatText:
			writeText(w, "text/plain; charset=utf-8", dialogue(res))
		default:
			render(w, named(res), format)
		}
	}
}

// named labels the segments by the voices' names, for the formats a person
// reads; the JSON keeps the ids and gives the names beside them.
func named(res *core.Result) *core.Result {
	names := map[string]string{}
	for _, sp := range res.Speakers {
		if sp.Name != "" {
			names[sp.ID] = sp.Name
		}
	}
	out := *res
	out.Segments = slices.Clone(res.Segments)
	for i, seg := range out.Segments {
		if seg.Speaker == nil {
			continue
		}
		if name, ok := names[*seg.Speaker]; ok {
			out.Segments[i].Speaker = &name
		}
	}
	return &out
}

// dialogue is the transcript as a person would write it down: who speaks,
// then what they say, a paragraph per turn.
func dialogue(res *core.Result) string {
	var b strings.Builder
	last := ""
	for _, seg := range named(res).Segments {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}
		who := ""
		if seg.Speaker != nil {
			who = *seg.Speaker
		}
		if who == last && b.Len() > 0 {
			b.WriteString(" " + text)
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		if who != "" {
			b.WriteString(who + ": ")
		}
		b.WriteString(text)
		last = who
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	return b.String()
}

func deleteIsolation(iso core.IsolationService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := iso.Delete(r.Context(), r.PathValue("id")); err != nil {
			WriteProblem(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
