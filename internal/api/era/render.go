package era

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/usunrise88/nanoasr/internal/api/subtitle"
	"github.com/usunrise88/nanoasr/internal/core"
)

// engineName is what the Asr-Engine response header carries. Upstream puts its
// engine choice there; ours has exactly one.
const engineName = "nanoasr"

// warningsHeader is this dialect's only channel for a degradation notice.
//
// The borrowed contract has no field for one: /asr returns an opaque body and
// /asr_task returns success plus a result. Dropping the warnings would break
// the rule the rest of the server keeps — that a request which got less than it
// asked for is told so — and adding a field would break the contract. A header
// does neither: a client that has never heard of it is unaffected.
const warningsHeader = "X-NanoASR-Warnings"

// writeTranscript renders a finished result in one of the upstream's five
// output formats.
//
// Every one of them is served as text/plain, json included. That is not an
// oversight copied from upstream: clients of this contract read the body as
// bytes and save it under the Content-Disposition filename, and answering
// application/json for one of the five would make that branch.
func writeTranscript(w http.ResponseWriter, res *core.Result, output, filename string) {
	body, err := renderTranscript(res, output)
	if err != nil {
		writeError(w, err)
		return
	}
	warnings := res.Warnings
	if output == outputJSON && missingWordConfidence(res) {
		warnings = append(warnings, core.Warning{
			Code: "word_confidence_unavailable",
			Message: "this model reports no per-word confidence, so every score " +
				"in the json output is 0",
		})
	}
	setWarnings(w, warnings)
	w.Header().Set("Asr-Engine", engineName)
	w.Header().Set("Content-Disposition", contentDisposition(filename, output))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
}

func renderTranscript(res *core.Result, output string) (string, error) {
	switch output {
	case outputVTT:
		return subtitle.VTT(res), nil
	case outputSRT:
		return subtitle.SRT(res), nil
	case outputTSV:
		return tsv(res), nil
	case outputJSON:
		return transcriptJSON(res)
	default:
		return txt(res), nil
	}
}

// txt is one segment per line.
//
// Not the whole transcript as a single paragraph: the reference service writes
// a line per segment, and a client that splits the body on newlines to get
// utterances would see one enormous utterance instead.
func txt(res *core.Result) string {
	var b strings.Builder
	for _, s := range res.Segments {
		b.WriteString(strings.TrimSpace(s.Text))
		b.WriteByte('\n')
	}
	return b.String()
}

// tsv is the tab-separated transcript: integer milliseconds and the segment
// text with tabs squeezed out.
//
// No header row. Whisper's own TSV writer emits "start end text" first, and the
// reference service this dialect replaces does not — a client reading the first
// line as data would take the header for a segment.
func tsv(res *core.Result) string {
	var b strings.Builder
	for _, s := range res.Segments {
		fmt.Fprintf(&b, "%d\t%d\t%s\n",
			millis(s.Start), millis(s.End),
			strings.ReplaceAll(strings.TrimSpace(s.Text), "\t", " "))
	}
	return b.String()
}

func millis(seconds float64) int64 {
	if seconds <= 0 {
		return 0
	}
	return int64(seconds*1000 + 0.5)
}

// transcriptJSON renders the shape the reference service returns.
//
// Deliberately not core.Result. This dialect exists so a client written against
// whisper-asr-webservice keeps working, and that client parses
// {segments, word_segments, language} with words carrying a "score" — a
// completely different document from the native result. Serving our own schema
// here would satisfy the contract's paths and headers and still break every
// caller at the first field access.
//
// Words are always present, with no flag to ask for them. The reference aligns
// every request whether or not word_timestamps was sent, so a client that omits
// the flag still expects them.
type jsonTranscript struct {
	Segments []jsonSegment `json:"segments"`
	// WordSegments is every word of the transcript in one flat list, which is
	// how the reference reports them alongside the per-segment copies.
	WordSegments []jsonWord `json:"word_segments"`
	Language     string     `json:"language"`
}

type jsonSegment struct {
	Start float64    `json:"start"`
	End   float64    `json:"end"`
	Text  string     `json:"text"`
	Words []jsonWord `json:"words"`
	// Speaker is additive and appears only when diarization ran. The reference
	// has no diarization at all, so no client of it can be relying on its
	// absence.
	Speaker *string `json:"speaker,omitempty"`
}

type jsonWord struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	// Score is the model's confidence in this word. It is always written, even
	// when the model reports none and the value is therefore 0: the field is
	// part of the schema, and a client indexing into it must not find nothing.
	// A result with no confidences at all is reported in X-NanoASR-Warnings.
	Score   float64 `json:"score"`
	Speaker *string `json:"speaker,omitempty"`
}

func transcriptJSON(res *core.Result) (string, error) {
	out := jsonTranscript{
		Segments:     make([]jsonSegment, 0, len(res.Segments)),
		WordSegments: []jsonWord{},
		Language:     res.Language,
	}
	for _, s := range res.Segments {
		seg := jsonSegment{
			Start:   s.Start,
			End:     s.End,
			Text:    strings.TrimSpace(s.Text),
			Words:   make([]jsonWord, 0, len(s.Words)),
			Speaker: s.Speaker,
		}
		for _, word := range s.Words {
			w := jsonWord{
				Word:    word.Word,
				Start:   word.Start,
				End:     word.End,
				Score:   word.Confidence,
				Speaker: word.Speaker,
			}
			seg.Words = append(seg.Words, w)
			out.WordSegments = append(out.WordSegments, w)
		}
		out.Segments = append(out.Segments, seg)
	}

	b, err := json.Marshal(out)
	if err != nil {
		return "", core.Errorf(core.CodeInternal, "cannot render the transcript as json").WithCause(err)
	}
	return string(b), nil
}

// missingWordConfidence reports whether the result carries no per-word
// confidence at all, which happens with models that do not produce one. The
// json schema still has a score field, so the zeroes have to be explained
// somewhere rather than read as "every word is certainly wrong".
func missingWordConfidence(res *core.Result) bool {
	seen := false
	for _, s := range res.Segments {
		for _, w := range s.Words {
			seen = true
			if w.Confidence > 0 {
				return false
			}
		}
	}
	return seen
}

// contentDisposition reproduces upstream's header byte for byte, percent-escape
// included: the filename travels inside a quoted string, so a name with a quote
// or a newline in it would otherwise be a header injection.
func contentDisposition(filename, output string) string {
	if filename == "" {
		filename = "audio"
	}
	return fmt.Sprintf(`attachment; filename="%s.%s"`, escapeFilename(filename), output)
}

// escapeFilename matches Python's urllib.parse.quote: unreserved characters and
// the marks quote leaves alone pass through, and every other byte becomes %XX.
//
// Byte by byte, and the encoding is written here rather than delegated to
// net/url on purpose. url.PathEscape takes a string, so feeding it one byte of
// a multi-byte character first widens that byte to a rune — which re-encodes it
// as two UTF-8 bytes and escapes both. A Cyrillic filename comes back through
// that path double-encoded.
func escapeFilename(name string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if isUnreservedFilenameByte(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

func isUnreservedFilenameByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '-', c == '_', c == '.', c == '~', c == '/':
		return true
	}
	return false
}

func setWarnings(w http.ResponseWriter, ws []core.Warning) {
	if len(ws) == 0 {
		return
	}
	codes := make([]string, 0, len(ws))
	for _, warning := range ws {
		codes = append(codes, warning.Code)
	}
	w.Header().Set(warningsHeader, strings.Join(codes, ","))
}

// --- envelopes --------------------------------------------------------------

// taskAccepted is the /asr_task response: success and the handle to poll with.
type taskAccepted struct {
	Success bool   `json:"success"`
	TaskID  string `json:"task_id"`
}

// taskState is the /asr_task/{id} response for everything except a finished,
// successful task — that one answers with the transcript itself.
//
// The field order and the success/is_ready pair are upstream's; success turns
// false only when the task finished and failed, which is why a task still
// running reports success true and is_ready false.
type taskState struct {
	Success bool       `json:"success"`
	IsReady bool       `json:"is_ready"`
	Error   *taskError `json:"error,omitempty"`
}

// taskError mirrors the exception upstream serialises.
//
// Traceback carries what this server actually knows — the domain error code and
// the parameter at fault — rather than a call stack. A stack would be the one
// thing in this contract that leaks the inside of the process to a client, and
// the field is a list of lines, so lines it is.
type taskError struct {
	Title     string   `json:"title"`
	Message   string   `json:"message"`
	Traceback []string `json:"traceback"`
}

func errorOf(job *core.Job) *taskError {
	if job.Error != nil {
		return &taskError{
			Title:     string(job.Error.Code),
			Message:   job.Error.Message,
			Traceback: traceback(job.Error),
		}
	}
	// Cancelled and expired jobs are terminal without an error of their own,
	// and answering "ready, failed, no reason" would be worse than saying which
	// terminal state it is.
	return &taskError{
		Title:     string(job.Status),
		Message:   fmt.Sprintf("the task is %s and produced no transcript", job.Status),
		Traceback: []string{"status: " + string(job.Status)},
	}
}

func traceback(e *core.Error) []string {
	out := []string{"code: " + string(e.Code)}
	if e.Param != "" {
		out = append(out, "param: "+e.Param)
	}
	return out
}

// errorEnvelope is FastAPI's shape, which is what a client of this contract
// already parses: the 404 upstream raises is {"detail": {"message": ...}}.
// Code is additive — a field a borrowed client ignores and an operator reading
// a log does not have to guess at.
type errorEnvelope struct {
	Detail errorDetail `json:"detail"`
}

type errorDetail struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}

func writeError(w http.ResponseWriter, err error) {
	e := core.AsError(err)
	writeJSON(w, e.Code.HTTPStatus(), errorEnvelope{Detail: errorDetail{
		Message: e.Message,
		Code:    string(e.Code),
		Param:   e.Param,
	}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
