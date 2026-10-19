package core

import (
	"context"
	"io"
	"time"
)

// IsolationService keeps one voice of a recording and removes the others.
//
// It is a conversation in three steps rather than a request: the server finds
// the voices, a person listens to them and picks one, and only then is that
// voice extracted. So unlike a transcription job, an isolation holds its audio
// between requests — the upload while the voices are found and picked, the
// extracted voice until it is fetched — for a bounded time, and for no longer
// than the server process lives. A nil IsolationService means the feature is
// not configured here.
type IsolationService interface {
	// Create accepts a recording and starts finding its voices.
	Create(ctx context.Context, req IsolationRequest) (*Isolation, error)
	Isolation(ctx context.Context, id string) (*Isolation, error)
	// Watch streams state changes as Service.Watch does for jobs: after is
	// the sequence number the client last saw, and the channel closes when
	// the isolation waits on a person or is over.
	Watch(ctx context.Context, id string, after int64) (<-chan IsolationEvent, error)
	// Sample is the voice sample of one speaker as WAV: exactly the audio the
	// extractor will be told to keep.
	Sample(ctx context.Context, id, speaker string) (io.ReadCloser, error)
	// Extract starts isolating the chosen voice. It may be called again once
	// that is done, for another voice or another sample of the same one.
	Extract(ctx context.Context, id string, sel IsolationSelection) (*Isolation, error)
	// Result is the isolated voice in the given format.
	Result(ctx context.Context, id string, format AudioFormat) (io.ReadCloser, error)
	// Transcribe submits the isolated voice as a transcription job.
	// req.Audio is ignored: the isolation supplies it.
	Transcribe(ctx context.Context, id string, req Request) (*Job, error)
	// Delete forgets the isolation and its audio at once.
	Delete(ctx context.Context, id string) error
	// Formats lists what Result can produce on this server.
	Formats() []AudioFormat
}

// AudioFormat is a container the isolated voice can be fetched in.
type AudioFormat string

const (
	AudioWAV AudioFormat = "wav"
	// AudioMP3 needs ffmpeg on the server.
	AudioMP3 AudioFormat = "mp3"
)

// ContentType is the media type of the format.
func (f AudioFormat) ContentType() string {
	if f == AudioMP3 {
		return "audio/mpeg"
	}
	return "audio/wav"
}

// IsolationStatus is where an isolation is.
type IsolationStatus string

const (
	IsolationQueued    IsolationStatus = "queued"
	IsolationAnalysing IsolationStatus = "analysing"
	// IsolationAwaitingSelection is a person's turn: the voices are found
	// and nothing happens until one is chosen.
	IsolationAwaitingSelection IsolationStatus = "awaiting_selection"
	IsolationExtracting        IsolationStatus = "extracting"
	IsolationDone              IsolationStatus = "done"
	IsolationFailed            IsolationStatus = "failed"
)

// Busy reports whether work is queued or running.
func (s IsolationStatus) Busy() bool {
	return s == IsolationQueued || s == IsolationAnalysing || s == IsolationExtracting
}

// IsolationRequest is a recording to isolate a voice from.
type IsolationRequest struct {
	Audio AudioSource
	// NumSpeakers is passed to the diarizer: 0 lets it decide.
	NumSpeakers int
	Source      Source
}

// IsolationSelection is the voice to keep.
type IsolationSelection struct {
	Speaker string `json:"speaker"`
	// SampleStart, when set, takes the 3-second sample from here instead of
	// the place the server chose, for when that one is not the voice wanted.
	SampleStart *float64 `json:"sample_start,omitempty"`
}

// Isolation is the state of one isolation, as a client sees it.
type Isolation struct {
	ID       string          `json:"id"`
	Status   IsolationStatus `json:"status"`
	Stage    string          `json:"stage,omitempty"`
	Percent  int             `json:"percent,omitempty"`
	Position int             `json:"position,omitempty"`
	Filename string          `json:"filename,omitempty"`
	Source   Source          `json:"source"`
	// Duration is the length of the recording, in seconds, once decoded.
	Duration float64 `json:"duration,omitempty"`

	Voices   []IsolationVoice    `json:"voices,omitempty"`
	Selected *IsolationSelection `json:"selected,omitempty"`
	Result   *IsolationResult    `json:"result,omitempty"`
	// Formats is what the result can be fetched as here.
	Formats  []AudioFormat `json:"formats,omitempty"`
	Warnings []Warning     `json:"warnings,omitempty"`
	Error    *Error        `json:"error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	// ExpiresAt is when the isolation and its audio are deleted, unless
	// something happens to it first; every change moves it on.
	ExpiresAt time.Time `json:"expires_at"`
}

// IsolationVoice is one voice the diarizer found.
type IsolationVoice struct {
	Speaker string `json:"speaker"`
	// TotalSpeech is how long it speaks, in seconds, and Turns in how many
	// turns.
	TotalSpeech float64 `json:"total_speech"`
	Turns       int     `json:"turns"`
	// Sample is the 3 seconds the extractor would keep this voice by; nil
	// when the voice cannot be isolated, and Reason says why.
	Sample *VoiceSample `json:"sample,omitempty"`
	Reason string       `json:"reason,omitempty"`
}

// VoiceSample describes a voice's sample. The audio is behind
// IsolationService.Sample.
type VoiceSample struct {
	// Kind is "window", 3 seconds from Start on, or "assembled" from the
	// voice's clean pieces where it never speaks alone that long.
	Kind  string  `json:"kind"`
	Start float64 `json:"start,omitempty"`
	// Similarity is how close the sample sounds to the voice as a whole.
	Similarity float64 `json:"similarity"`
}

// IsolationResult describes the isolated voice. The audio is behind
// IsolationService.Result.
type IsolationResult struct {
	Speaker string `json:"speaker"`
	// Name is what the voice is called when downloaded or transcribed,
	// without an extension: the upload's name with the speaker in it.
	Name       string  `json:"name"`
	Duration   float64 `json:"duration"`
	SampleRate int     `json:"sample_rate"`
	// JobID is the last transcription submitted from it, if any.
	JobID string `json:"job_id,omitempty"`
}

// IsolationEvent is one state transition.
type IsolationEvent struct {
	Seq       int64     `json:"seq"`
	Isolation Isolation `json:"isolation"`
}
