package openai

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/usunrise88/nanoasr/internal/api/subtitle"
	"github.com/usunrise88/nanoasr/internal/core"
)

// Response formats accepted by /v1/audio/transcriptions.
const (
	formatJSON        = "json"
	formatVerboseJSON = "verbose_json"
	formatText        = "text"
	formatSRT         = "srt"
	formatVTT         = "vtt"
)

func validFormat(f string) bool {
	switch f {
	case formatJSON, formatVerboseJSON, formatText, formatSRT, formatVTT:
		return true
	}
	return false
}

// simpleResponse is what response_format=json returns: OpenAI sends the text
// and nothing else, and clients rely on that.
type simpleResponse struct {
	Text string `json:"text"`
}

// verboseResponse follows OpenAI's verbose_json, plus fields of our own.
//
// The extra fields are additive — timestamp_source, silence, warnings and
// stats — because an OpenAI client ignores keys it does not know, and dropping
// them would mean a caller could not tell model-provided timings from VAD
// fallbacks.
type verboseResponse struct {
	Task     string  `json:"task"`
	Language string  `json:"language"`
	Duration float64 `json:"duration"`
	Text     string  `json:"text"`

	Segments []verboseSegment `json:"segments,omitempty"`
	Words    []verboseWord    `json:"words,omitempty"`

	TimestampSource core.TimestampSource `json:"timestamp_source"`
	Silence         []core.Silence       `json:"silence,omitempty"`
	Warnings        []core.Warning       `json:"warnings,omitempty"`
	Stats           *core.Stats          `json:"stats,omitempty"`
}

type verboseSegment struct {
	ID      int     `json:"id"`
	Seek    int     `json:"seek"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Text    string  `json:"text"`
	Speaker *string `json:"speaker,omitempty"`
	// AvgLogprob is OpenAI's field name. Ours is a geometric mean of token
	// probabilities converted back to a log, and it is not calibrated.
	AvgLogprob float64 `json:"avg_logprob"`
}

type verboseWord struct {
	Word       string  `json:"word"`
	Start      float64 `json:"start"`
	End        float64 `json:"end"`
	Confidence float64 `json:"confidence,omitempty"`
	Speaker    *string `json:"speaker,omitempty"`
	// SpeakerConfidence is how much of this word the winning speaker turn
	// covered. Additive, like the rest: an OpenAI client ignores it.
	SpeakerConfidence float64 `json:"speaker_confidence,omitempty"`
	Channel           int     `json:"channel,omitempty"`
}

// render writes the result in the requested format.
func render(w http.ResponseWriter, res *core.Result, format string, wantWords bool) {
	switch format {
	case formatText:
		writeText(w, "text/plain; charset=utf-8", res.Text+"\n")
	case formatSRT:
		writeText(w, subtitle.SRTContentType, subtitle.SRT(res))
	case formatVTT:
		writeText(w, subtitle.VTTContentType, subtitle.VTT(res))
	case formatVerboseJSON:
		writeJSON(w, http.StatusOK, buildVerbose(res, wantWords))
	default:
		writeJSON(w, http.StatusOK, simpleResponse{Text: res.Text})
	}
}

func buildVerbose(res *core.Result, wantWords bool) verboseResponse {
	out := verboseResponse{
		Task:            "transcribe",
		Language:        res.Language,
		Duration:        res.Duration,
		Text:            res.Text,
		TimestampSource: res.TimestampSource,
		Silence:         res.Silence,
		Warnings:        res.Warnings,
		Stats:           &res.Stats,
	}

	for _, s := range res.Segments {
		out.Segments = append(out.Segments, verboseSegment{
			ID:         s.ID,
			Seek:       int(s.Start * 100), // OpenAI reports seek in centiseconds
			Start:      s.Start,
			End:        s.End,
			Text:       s.Text,
			Speaker:    s.Speaker,
			AvgLogprob: logprob(s.AvgConfidence),
		})
		if !wantWords {
			continue
		}
		for _, word := range s.Words {
			out.Words = append(out.Words, verboseWord{
				Word:              word.Word,
				Start:             word.Start,
				End:               word.End,
				Confidence:        word.Confidence,
				Speaker:           wordSpeaker(word, s),
				SpeakerConfidence: word.SpeakerConfidence,
				Channel:           word.Channel,
			})
		}
	}
	return out
}

// logprob converts our geometric-mean confidence back to the log domain so the
// field means roughly what an OpenAI client expects to find there.
func logprob(confidence float64) float64 {
	if confidence <= 0 {
		return 0
	}
	return ln(confidence)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeText(w http.ResponseWriter, contentType, body string) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
}

// wordSpeaker prefers the word's own attribution over its segment's.
//
// They agree once diarization has split segments at turn boundaries. They do
// not agree when a result was stored before that happened, and the word is the
// more specific of the two.
func wordSpeaker(w core.Word, s core.Segment) *string {
	if w.Speaker != nil {
		return w.Speaker
	}
	return s.Speaker
}
