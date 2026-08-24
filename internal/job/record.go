package job

import (
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Record is one row of the jobs table: the client-facing core.Job plus what the
// server needs to run the work and nothing the client should see.
//
// The split exists because core.Job is a wire shape — it is rendered straight
// into an API response — while resuming a queued job after a restart needs the
// decoding parameters, the owning key and the priority, none of which belong in
// that response.
type Record struct {
	Job      core.Job
	Params   Params
	Priority Priority
	APIKeyID string

	AudioBytes   int64
	AudioSeconds float64
}

// Params is core.Request without the audio.
//
// AudioSource is an interface over an open file; it cannot be serialised and
// does not need to be. The spooled upload is found by the job id (see spool.go),
// so what has to survive a restart is only the decoding parameters.
type Params struct {
	ModelID  string `json:"model_id,omitempty"`
	Language string `json:"language,omitempty"`

	ChannelMode    core.ChannelMode `json:"channel_mode,omitempty"`
	DecodingMethod string           `json:"decoding_method,omitempty"`
	MaxActivePaths int              `json:"max_active_paths,omitempty"`

	Diarize     bool `json:"diarize,omitempty"`
	NumSpeakers int  `json:"num_speakers,omitempty"`

	Punctuate bool `json:"punctuate,omitempty"`
	ITN       bool `json:"itn,omitempty"`

	Hotwords      []string `json:"hotwords,omitempty"`
	HotwordsScore float32  `json:"hotwords_score,omitempty"`

	Strict     bool        `json:"strict,omitempty"`
	Source     core.Source `json:"source,omitempty"`
	WebhookURL string      `json:"webhook_url,omitempty"`
}

// ParamsOf projects a request for storage.
func ParamsOf(req core.Request) Params {
	return Params{
		ModelID:        req.ModelID,
		Language:       req.Language,
		ChannelMode:    req.ChannelMode,
		DecodingMethod: req.DecodingMethod,
		MaxActivePaths: req.MaxActivePaths,
		Diarize:        req.Diarize,
		NumSpeakers:    req.NumSpeakers,
		Punctuate:      req.Punctuate,
		ITN:            req.ITN,
		Hotwords:       req.Hotwords,
		HotwordsScore:  req.HotwordsScore,
		Strict:         req.Strict,
		Source:         req.Source,
		WebhookURL:     req.WebhookURL,
	}
}

// Request rebuilds a runnable request around a reopened audio source.
func (p Params) Request(audio core.AudioSource, apiKeyID string) core.Request {
	return core.Request{
		Audio:          audio,
		ModelID:        p.ModelID,
		Language:       p.Language,
		ChannelMode:    p.ChannelMode,
		DecodingMethod: p.DecodingMethod,
		MaxActivePaths: p.MaxActivePaths,
		Diarize:        p.Diarize,
		NumSpeakers:    p.NumSpeakers,
		Punctuate:      p.Punctuate,
		ITN:            p.ITN,
		Hotwords:       p.Hotwords,
		HotwordsScore:  p.HotwordsScore,
		Strict:         p.Strict,
		Source:         p.Source,
		APIKeyID:       apiKeyID,
		WebhookURL:     p.WebhookURL,
	}
}

// NewRecord builds the queued record for a freshly accepted request.
func NewRecord(id string, req core.Request, priority Priority, now time.Time) Record {
	var bytes int64
	var filename string
	if req.Audio != nil {
		bytes = req.Audio.Size()
		filename = req.Audio.Filename()
	}
	return Record{
		Job: core.Job{
			ID:        id,
			Status:    core.JobQueued,
			ModelID:   req.ModelID,
			Filename:  filename,
			Source:    req.Source,
			CreatedAt: now,
		},
		Params:     ParamsOf(req),
		Priority:   priority,
		APIKeyID:   req.APIKeyID,
		AudioBytes: bytes,
	}
}

// Terminal reports whether a status can still change.
func Terminal(s core.JobStatus) bool {
	switch s {
	case core.JobSucceeded, core.JobFailed, core.JobCanceled, core.JobExpired:
		return true
	}
	return false
}
