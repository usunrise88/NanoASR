package era

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/usunrise88/nanoasr/internal/core"
)

// engineName is what the Asr-Engine response header carries.
//
// The reference puts its ASR_ENGINE setting there, so the service this dialect
// replaces answers "whisperx". Ours says what actually produced the transcript.
// Echoing "whisperx" would be the one claim in this contract a client cannot
// check and cannot recover from being wrong about — the same reason
// task=translate is refused rather than answered with a transcription.
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
		return vtt(res), nil
	case outputSRT:
		return srt(res), nil
	case outputTSV:
		return tsv(res), nil
	case outputJSON:
		return transcriptJSON(res)
	default:
		// Anything else, including an output the caller invented, is txt. The
		// reference declares an enum and then does not enforce it: its writer
		// picks by name and falls through to WriteTXT, so a request that would
		// have been answered there is answered here too.
		return txt(res), nil
	}
}

// txt is one segment per line.
//
// Not the whole transcript as a single paragraph: the reference service writes
// a line per segment, and a client that splits the body on newlines to get
// utterances would see one enormous utterance instead. A diarized segment
// carries the same "[spk]: " prefix whisperx writes.
func txt(res *core.Result) string {
	var b strings.Builder
	for _, s := range res.Segments {
		b.WriteString(withSpeaker(strings.TrimSpace(s.Text), s.Speaker))
		b.WriteByte('\n')
	}
	return b.String()
}

// tsv is the tab-separated transcript: integer milliseconds and the segment
// text with tabs squeezed out.
//
// The header row is part of it. Every writer the reference service can reach —
// whisperx's, faster-whisper's and whisper's own — prints "start end text"
// before the first segment, so a client that skips the first line as a header
// would otherwise lose a segment here.
func tsv(res *core.Result) string {
	var b strings.Builder
	b.WriteString("start\tend\ttext\n")
	for _, s := range res.Segments {
		fmt.Fprintf(&b, "%d\t%d\t%s\n",
			millis(s.Start), millis(s.End),
			strings.ReplaceAll(strings.TrimSpace(s.Text), "\t", " "))
	}
	return b.String()
}

// millis matches the reference's round(1000 * start): Python rounds halves to
// even, which RoundToEven is, and the difference only ever shows on a timing
// that lands exactly on half a millisecond.
func millis(seconds float64) int64 {
	if seconds <= 0 {
		return 0
	}
	return int64(math.RoundToEven(seconds * 1000))
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
	Start jsonFloat  `json:"start"`
	End   jsonFloat  `json:"end"`
	Text  string     `json:"text"`
	Words []jsonWord `json:"words"`
	// Speaker is additive and appears only when diarization ran. The
	// replaced service runs whisperx with no HF_TOKEN, so it does not even
	// publish diarize and no client of it can be relying on the field.
	Speaker *string `json:"speaker,omitempty"`
}

type jsonWord struct {
	Word  string    `json:"word"`
	Start jsonFloat `json:"start"`
	End   jsonFloat `json:"end"`
	// Score is the model's confidence in this word. It is always written, even
	// when the model reports none and the value is therefore 0: the field is
	// part of the schema, and a client indexing into it must not find nothing.
	// A result with no confidences at all is reported in X-NanoASR-Warnings.
	Score   jsonFloat `json:"score"`
	Speaker *string   `json:"speaker,omitempty"`
}

// jsonFloat prints a number the way Python's json.dump does.
//
// Go writes float64(3) as "3", and a Python client reads that back as an int:
// json.loads("3") is 3 where json.loads("3.0") is 3.0. A timing that happened to
// land on a whole second would change type under the client, which is exactly
// the kind of break this dialect exists to prevent. The rest is Python's repr:
// the shortest decimal that round-trips, which is what 'f' with a precision of
// -1 produces for every value this schema can hold.
type jsonFloat float64

func (f jsonFloat) MarshalJSON() ([]byte, error) {
	s := strconv.FormatFloat(float64(f), 'f', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return []byte(s), nil
}

func transcriptJSON(res *core.Result) (string, error) {
	out := jsonTranscript{
		Segments:     make([]jsonSegment, 0, len(res.Segments)),
		WordSegments: []jsonWord{},
		Language:     res.Language,
	}
	for _, s := range res.Segments {
		seg := jsonSegment{
			Start:   milliRound(s.Start),
			End:     milliRound(s.End),
			Text:    strings.TrimSpace(s.Text),
			Words:   make([]jsonWord, 0, len(s.Words)),
			Speaker: s.Speaker,
		}
		for _, word := range s.Words {
			w := jsonWord{
				Word:    word.Word,
				Start:   milliRound(word.Start),
				End:     milliRound(word.End),
				Score:   milliRound(word.Confidence),
				Speaker: word.Speaker,
			}
			seg.Words = append(seg.Words, w)
			out.WordSegments = append(out.WordSegments, w)
		}
		out.Segments = append(out.Segments, seg)
	}

	// SetEscapeHTML is off because the reference dumps with ensure_ascii=False
	// and no escaping of its own: a transcript containing "&" or "<" would
	// otherwise travel as \u0026 and \u003c, which decodes the same and does not
	// compare the same.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return "", core.Errorf(core.CodeInternal, "cannot render the transcript as json").WithCause(err)
	}
	// Encode writes a trailing newline; json.dump does not.
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// milliRound is whisperx's round(x, 3), which is what its aligner writes for
// every word start, end and score.
//
// Without it a timing that arrived as a float32 is printed as the float64 it
// widens to — 0.9199999928474426 where the reference wrote 0.92. The value is
// the same to within a microsecond either way; the bytes a client diffs, logs
// or compares are not.
func milliRound(v float64) jsonFloat {
	return jsonFloat(math.Round(v*1000) / 1000)
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
//
// The extension is escaped the same way, which upstream does not do. Since an
// output outside the enum is no longer refused, the caller chooses this half of
// the header too, and a value like `"; x="` would end the quoted string early.
// For each of the five real formats the escape is the identity, so the header is
// unchanged for every request that was ever valid.
func contentDisposition(filename, output string) string {
	if filename == "" {
		filename = "audio"
	}
	return fmt.Sprintf(`attachment; filename="%s.%s"`,
		escapeFilename(filename), escapeFilename(output))
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

// errorEnvelope is the shape the reference raises by hand, which is what a
// client of this contract already parses: its 404 is {"detail": {"message":
// ...}}. Code is additive — a field a borrowed client ignores and an operator
// reading a log does not have to guess at.
type errorEnvelope struct {
	Detail errorDetail `json:"detail"`
}

type errorDetail struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}

// validationEnvelope is the other error shape this contract has: the 422
// FastAPI generates when a request does not satisfy the endpoint's signature.
//
// It is a list, not an object, and a client that reads detail[0].msg off a
// rejected upload finds nothing if we answer with errorEnvelope instead. The
// field names are pydantic v1's, which is what the reference service was
// measured returning:
//
//	{"detail":[{"loc":["body","audio_file"],"msg":"field required",
//	            "type":"value_error.missing"}]}
type validationEnvelope struct {
	Detail []validationDetail `json:"detail"`
}

type validationDetail struct {
	Loc  []string `json:"loc"`
	Msg  string   `json:"msg"`
	Type string   `json:"type"`
}

// missingFieldType is pydantic v1's tag for a required field that was not sent.
const missingFieldType = "value_error.missing"

func writeError(w http.ResponseWriter, err error) {
	e := core.AsError(err)
	// A bad request that names the field at fault is a validation failure, and
	// this contract reports those as 422 with a list. Everything else — a
	// missing task, a model that cannot be loaded, an upload over the limit —
	// keeps the object shape and its own status.
	if e.Code == core.CodeInvalidRequest && e.Param != "" {
		writeJSON(w, http.StatusUnprocessableEntity,
			validationEnvelope{Detail: []validationDetail{validationDetailOf(e)}})
		return
	}
	writeJSON(w, e.Code.HTTPStatus(), errorEnvelope{Detail: errorDetail{
		Message: e.Message,
		Code:    string(e.Code),
		Param:   e.Param,
	}})
}

// validationDetailOf places the fault where FastAPI would have placed it: the
// file is a body field and everything else in this contract is a query
// parameter.
func validationDetailOf(e *core.Error) validationDetail {
	if e.Param == uploadField {
		return validationDetail{
			Loc:  []string{"body", uploadField},
			Msg:  "field required",
			Type: missingFieldType,
		}
	}
	return validationDetail{Loc: []string{"query", e.Param}, Msg: e.Message, Type: "value_error"}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
