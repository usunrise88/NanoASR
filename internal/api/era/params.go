package era

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Output formats, matching the enum the upstream service publishes.
const (
	outputTXT  = "txt"
	outputVTT  = "vtt"
	outputSRT  = "srt"
	outputTSV  = "tsv"
	outputJSON = "json"

	defaultOutput = outputTXT
)

func validOutput(v string) bool {
	switch v {
	case outputTXT, outputVTT, outputSRT, outputTSV, outputJSON:
		return true
	}
	return false
}

// Tasks the upstream service publishes. Only one of them is a thing this build
// can do; see params.parse.
const (
	taskTranscribe = "transcribe"
	taskTranslate  = "translate"
)

// params is one /asr or /asr_task request after validation.
//
// It carries warnings as well as a request because several of the upstream
// parameters have no exact equivalent here and are honoured approximately. The
// rule the rest of this server follows — never degrade silently — applies just
// as much to a borrowed contract, so every approximation is named.
type params struct {
	request core.Request
	output  string
	// wantWords decides whether the json output carries word timings. The
	// pipeline produces them either way — this only chooses what is rendered,
	// which is what the upstream flag does too.
	wantWords bool
	warn      []core.Warning
}

func parseParams(r *http.Request, source core.AudioSource) (params, error) {
	p := params{
		output: defaultOutput,
		request: core.Request{
			Audio:  source,
			Source: core.SourceAPI,
		},
	}

	if v := value(r, "output"); v != "" {
		if !validOutput(v) {
			return p, invalid("output",
				"output %q is not one of txt, vtt, srt, tsv, json", v)
		}
		p.output = v
	}

	// translate is refused rather than approximated: answering a translation
	// request with a transcription would be the one degradation a caller
	// cannot detect from the response body.
	switch task := value(r, "task"); task {
	case "", taskTranscribe:
	case taskTranslate:
		return p, core.Errorf(core.CodeNotImplemented,
			"task=translate is not supported: this build ships transcription models only").
			WithParam("task")
	default:
		return p, invalid("task", "task %q is not one of transcribe, translate", task)
	}

	p.request.Language = value(r, "language")

	// The upstream initial_prompt conditions a language model. The nearest
	// honest equivalent here is hotword biasing, exactly as the OpenAI dialect
	// maps prompt, and the difference is reported rather than assumed away.
	if prompt := value(r, "initial_prompt"); prompt != "" {
		for _, word := range strings.Split(prompt, ",") {
			if w := strings.TrimSpace(word); w != "" {
				p.request.Hotwords = append(p.request.Hotwords, w)
			}
		}
		p.warn = append(p.warn, core.Warning{
			Code:    "initial_prompt_mapped_to_hotwords",
			Message: "initial_prompt was read as a comma-separated hotword list, not as an LM prompt",
		})
	}

	diarize, err := boolValue(r, "diarize")
	if err != nil {
		return p, err
	}
	p.request.Diarize = diarize

	minSpeakers, err := intValue(r, "min_speakers")
	if err != nil {
		return p, err
	}
	maxSpeakers, err := intValue(r, "max_speakers")
	if err != nil {
		return p, err
	}
	p.applySpeakerRange(minSpeakers, maxSpeakers)

	// Parameters this pipeline cannot vary per request. They are accepted so a
	// client written against the upstream service keeps working, and each one
	// warns only when the caller actually sent it: warning on a default nobody
	// typed would make every response noisy and teach clients to ignore the
	// header the warnings arrive in.
	if v, err := boolValue(r, "word_timestamps"); err != nil {
		return p, err
	} else if v {
		// Not inert: it decides whether the json output carries words.
		p.wantWords = true
	}

	if present(r, "vad_filter") {
		p.warn = append(p.warn, core.Warning{
			Code: "vad_filter_ignored",
			Message: "voice activity detection is how this pipeline finds segments at all, " +
				"so it is a server setting (vad.enabled) rather than a request option",
		})
	}
	if present(r, "encode") {
		p.warn = append(p.warn, core.Warning{
			Code: "encode_ignored",
			Message: "the decoder chooses its own path per format: WAV and raw PCM are " +
				"decoded in-process and everything else goes through ffmpeg",
		})
	}
	return p, nil
}

// applySpeakerRange folds the upstream min/max pair onto the one number this
// clusterer takes.
//
// An exact count is the only part of the range that survives, and that is not a
// shortcut: sherpa-onnx cuts a single dendrogram either at a height or at
// exactly N leaves, so a range has nothing to cut at. Reducing it to threshold
// clustering is also frequently the better answer (SPEC decision №39), which is
// why this is a warning and not a refusal.
func (p *params) applySpeakerRange(minSpeakers, maxSpeakers int) {
	switch {
	case minSpeakers <= 0 && maxSpeakers <= 0:
		return
	case minSpeakers > 0 && minSpeakers == maxSpeakers:
		p.request.NumSpeakers = minSpeakers
	default:
		p.warn = append(p.warn, core.Warning{
			Code: "speaker_range_reduced",
			Message: "clustering takes an exact speaker count or none at all, so a " +
				"min/max range was dropped; send min_speakers and max_speakers " +
				"equal to force a count",
		})
	}
}

func present(r *http.Request, name string) bool {
	if r.URL.Query().Has(name) {
		return true
	}
	return r.MultipartForm != nil && len(r.MultipartForm.Value[name]) > 0
}

// value reads a query parameter, falling back to the multipart body.
//
// The upstream service declares these as query parameters and reads nothing
// else. Accepting the form as well costs nothing and rescues the client that
// put them where the file went.
func value(r *http.Request, name string) string {
	if v := r.URL.Query().Get(name); v != "" {
		return strings.TrimSpace(v)
	}
	if r.MultipartForm != nil {
		if vs := r.MultipartForm.Value[name]; len(vs) > 0 {
			return strings.TrimSpace(vs[0])
		}
	}
	return ""
}

func boolValue(r *http.Request, name string) (bool, error) {
	v := value(r, name)
	if v == "" {
		return false, nil
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, invalid(name, "%s must be a boolean, got %q", name, v)
}

func intValue(r *http.Request, name string) (int, error) {
	v := value(r, name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, invalid(name, "%s must be an integer, got %q", name, v)
	}
	if n < 0 {
		return 0, invalid(name, "%s must not be negative", name)
	}
	return n, nil
}

func invalid(param, format string, args ...any) error {
	return core.Errorf(core.CodeInvalidRequest, format, args...).WithParam(param)
}
