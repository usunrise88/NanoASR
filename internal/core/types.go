// Package core holds the domain model of NanoASR and the service contract that
// every API dialect talks to. Nothing in this package knows about HTTP,
// multipart forms, JSON tags of a particular dialect or SSE: adapters translate.
package core

import (
	"io"
	"time"
)

// ChannelMode decides what happens to a multi-channel recording.
type ChannelMode string

const (
	// ChannelDownmix averages every channel into one. Default.
	ChannelDownmix ChannelMode = "downmix"
	// ChannelFirst keeps channel 0 and drops the rest.
	ChannelFirst ChannelMode = "first"
	// ChannelSplit recognises each channel independently and merges by time.
	// For two-legged telephony this is a cheaper and more accurate substitute
	// for diarization.
	ChannelSplit ChannelMode = "split"
)

// TimestampSource records how word timings were obtained, so a client can tell
// model-provided timings from VAD-segment fallbacks.
type TimestampSource string

const (
	TimestampToken   TimestampSource = "token"   // from the acoustic model
	TimestampSegment TimestampSource = "segment" // VAD boundaries only
	TimestampAligned TimestampSource = "aligned" // forced alignment (M6)
)

// Source distinguishes API traffic from the built-in test UI.
type Source string

const (
	SourceAPI Source = "api"
	SourceUI  Source = "ui"
)

// AudioSource is the uploaded payload. Implementations must be re-openable so
// a job can be retried, and must clean up after themselves on Close.
type AudioSource interface {
	Open() (io.ReadCloser, error)
	Filename() string
	Size() int64
	Close() error
}

// Request is a single transcription request, already validated and free of any
// transport concerns.
type Request struct {
	Audio AudioSource

	ModelID  string // empty → config default
	Language string // BCP-47-ish; validated against the model manifest

	ChannelMode    ChannelMode
	DecodingMethod string // greedy_search | modified_beam_search
	MaxActivePaths int

	Diarize     bool
	NumSpeakers int // 0 → cluster by threshold

	Punctuate bool
	ITN       bool

	Hotwords      []string
	HotwordsScore float32

	// Strict turns capability downgrades into errors instead of warnings.
	Strict bool

	Source     Source
	APIKeyID   string
	WebhookURL string
}

// Word is the smallest unit we report and the reason word_timestamps exists.
type Word struct {
	Word       string  `json:"word"`
	Start      float64 `json:"start"`
	End        float64 `json:"end"`
	Confidence float64 `json:"confidence,omitempty"`
	// Original keeps the pre-ITN surface form when inverse text normalisation
	// rewrote this span.
	Original string `json:"original,omitempty"`
}

// Segment is one VAD-delimited stretch of speech.
type Segment struct {
	ID            int     `json:"id"`
	Start         float64 `json:"start"`
	End           float64 `json:"end"`
	Text          string  `json:"text"`
	Channel       int     `json:"channel"`
	Speaker       *string `json:"speaker"`
	AvgConfidence float64 `json:"avg_confidence,omitempty"`
	Words         []Word  `json:"words,omitempty"`
}

// Silence is the complement of speech; the UI player paints these.
type Silence struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// Speaker summarises one diarization cluster.
type Speaker struct {
	ID          string  `json:"id"`
	TotalSpeech float64 `json:"total_speech"`
	Segments    int     `json:"segments"`
}

// Stats is what you would otherwise have to guess without metrics.
type Stats struct {
	AudioDuration float64          `json:"audio_duration"`
	ProcessingMS  int64            `json:"processing_ms"`
	RTF           float64          `json:"rtf"`
	StagesMS      map[string]int64 `json:"stages_ms"`
	SegmentsTotal int              `json:"segments_total"`
	SpeechRatio   float64          `json:"speech_ratio"`
}

// Warning reports a successful-but-degraded outcome. Never an error.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Result is the canonical transcription result. Dialects project it; they never
// extend it.
type Result struct {
	ID              string          `json:"id"`
	Model           string          `json:"model"`
	Language        string          `json:"language"`
	Duration        float64         `json:"duration"`
	Text            string          `json:"text"`
	TimestampSource TimestampSource `json:"timestamp_source"`
	Segments        []Segment       `json:"segments"`
	Silence         []Silence       `json:"silence"`
	Speakers        []Speaker       `json:"speakers,omitempty"`
	Stats           Stats           `json:"stats"`
	Warnings        []Warning       `json:"warnings,omitempty"`
}

// Words flattens every segment into one slice, in time order. The UI player
// binary-searches this to highlight the current word.
func (r *Result) Words() []Word {
	n := 0
	for i := range r.Segments {
		n += len(r.Segments[i].Words)
	}
	out := make([]Word, 0, n)
	for i := range r.Segments {
		out = append(out, r.Segments[i].Words...)
	}
	return out
}

// JobStatus is the lifecycle of an asynchronous request.
type JobStatus string

const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
	JobCanceled  JobStatus = "canceled"
	JobExpired   JobStatus = "expired"
)

// Job is one queued or completed unit of work.
type Job struct {
	ID         string     `json:"id"`
	Status     JobStatus  `json:"status"`
	Position   int        `json:"position,omitempty"`
	Stage      string     `json:"stage,omitempty"`
	Percent    int        `json:"percent,omitempty"`
	ModelID    string     `json:"model_id"`
	ModelRev   string     `json:"model_rev"`
	Filename   string     `json:"filename,omitempty"`
	Source     Source     `json:"source"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Result     *Result    `json:"result,omitempty"`
	Error      *Error     `json:"error,omitempty"`
}

// JobFilter narrows a history listing.
type JobFilter struct {
	Status  []JobStatus
	ModelID string
	Source  Source
	Since   *time.Time
	Limit   int
	Cursor  string
}
