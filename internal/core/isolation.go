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
// extracted voices until they are fetched — for a bounded time, and for no
// longer than the server process lives, unless the person asks for it to be
// kept. A nil IsolationService means the feature is not configured here.
type IsolationService interface {
	// Create accepts a recording and starts finding its voices.
	Create(ctx context.Context, req IsolationRequest) (*Isolation, error)
	// List is every isolation the caller may see, the newest first.
	List(ctx context.Context) ([]Isolation, error)
	Isolation(ctx context.Context, id string) (*Isolation, error)
	// Watch streams state changes as Service.Watch does for jobs: after is
	// the sequence number the client last saw, and the channel closes when
	// the isolation waits on a person or is over.
	Watch(ctx context.Context, id string, after int64) (<-chan IsolationEvent, error)
	// Sample is the voice sample of one speaker as WAV: exactly the audio the
	// extractor will be told to keep.
	Sample(ctx context.Context, id, speaker string) (io.ReadCloser, error)
	// Update names the voices, and keeps the isolation or lets it expire.
	Update(ctx context.Context, id string, upd IsolationUpdate) (*Isolation, error)
	// Extract starts isolating the chosen voice. It may be called again once
	// that is done, for another voice or another sample of the same one; a
	// voice isolated again replaces its own track and no other.
	Extract(ctx context.Context, id string, sel IsolationSelection) (*Isolation, error)
	// ExtractAll isolates every voice that has a sample and no track yet,
	// one track per voice.
	ExtractAll(ctx context.Context, id string) (*Isolation, error)
	// Audio is the recording itself, as the isolation holds it: mono, at
	// the extractor's rate, on the same timeline as every track.
	Audio(ctx context.Context, id string, format AudioFormat) (io.ReadCloser, error)
	// Peaks is the waveform of the recording (an empty speaker) or of one
	// voice's track, for a player that streams the audio rather than
	// downloading and decoding all of it to draw one.
	Peaks(ctx context.Context, id, speaker string, buckets int) (*AudioPeaks, error)
	// Result is one voice's track in the given format. An empty speaker is
	// the voice chosen last, or the only track there is.
	Result(ctx context.Context, id, speaker string, format AudioFormat) (io.ReadCloser, error)
	// Transcribe submits one track as a transcription job, speaker as for
	// Result. req.Audio is ignored: the isolation supplies it.
	Transcribe(ctx context.Context, id, speaker string, req Request) (*Job, error)
	// TranscribeSpeakers isolates every voice that has no track yet,
	// transcribes every track as a job of its own and merges the jobs into
	// one transcript, each word labelled with the voice it came from.
	// req.Audio and req.Diarize are ignored: a track is one voice.
	TranscribeSpeakers(ctx context.Context, id string, req Request) (*Isolation, error)
	// Transcript is the merged transcript TranscribeSpeakers made, with
	// the voices' names as they are now.
	Transcript(ctx context.Context, id string) (*Result, error)
	// Delete forgets the isolation and its audio at once, kept or not.
	Delete(ctx context.Context, id string) error
	// Formats lists what Result can produce on this server.
	Formats() []AudioFormat
	// CanKeep reports whether an isolation may be kept here past its TTL.
	CanKeep() bool
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

// AudioPeaks is a waveform: the loudness of equal stretches of the audio, RMS,
// scaled so the loudest is 1.
type AudioPeaks struct {
	Duration float64   `json:"duration"`
	Values   []float32 `json:"values"`
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
	// IsolationTranscribing is TranscribeSpeakers waiting on its jobs.
	IsolationTranscribing IsolationStatus = "transcribing"
	IsolationDone         IsolationStatus = "done"
	IsolationFailed       IsolationStatus = "failed"
)

// Busy reports whether work is queued or running.
func (s IsolationStatus) Busy() bool {
	return s == IsolationQueued || s == IsolationAnalysing || s == IsolationExtracting ||
		s == IsolationTranscribing
}

// IsolationRequest is a recording to isolate a voice from.
type IsolationRequest struct {
	Audio AudioSource
	// NumSpeakers is passed to the diarizer: 0 lets it decide.
	NumSpeakers int
	// Keep stores the isolation until it is deleted, across restarts,
	// instead of letting it expire.
	Keep   bool
	Source Source
}

// IsolationUpdate changes what a person decides about an isolation; a field
// left out is left as it is.
type IsolationUpdate struct {
	// Keep stores the isolation until it is deleted, or, false, lets it
	// expire again.
	Keep *bool `json:"keep,omitempty"`
	// Names maps a speaker to what to call it; an empty name restores the
	// speaker's own id.
	Names map[string]string `json:"names,omitempty"`
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
	// Voice is the speaker the running stage works on, when it works on one.
	Voice    string `json:"voice,omitempty"`
	Filename string `json:"filename,omitempty"`
	Source   Source `json:"source"`
	// Duration is the length of the recording, in seconds, once decoded.
	Duration float64 `json:"duration,omitempty"`
	// Keep says the isolation is stored until it is deleted.
	Keep bool `json:"keep"`

	Voices   []IsolationVoice    `json:"voices,omitempty"`
	Selected *IsolationSelection `json:"selected,omitempty"`
	// Result is the track of the voice chosen last.
	Result *IsolationResult `json:"result,omitempty"`
	// Tracks are every voice isolated so far, in the order of Voices.
	Tracks     []IsolationResult    `json:"tracks,omitempty"`
	Transcript *IsolationTranscript `json:"transcript,omitempty"`
	// Formats is what the tracks can be fetched as here.
	Formats  []AudioFormat `json:"formats,omitempty"`
	Warnings []Warning     `json:"warnings,omitempty"`
	Error    *Error        `json:"error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	// ExpiresAt is when the isolation and its audio are deleted, unless
	// something happens to it first; every change moves it on. A kept
	// isolation has none.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// IsolationVoice is one voice the diarizer found.
type IsolationVoice struct {
	Speaker string `json:"speaker"`
	// Name is what a person called the voice; empty until somebody does.
	Name string `json:"name,omitempty"`
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

// IsolationResult describes one isolated voice, a track. The audio is behind
// IsolationService.Result.
type IsolationResult struct {
	Speaker string `json:"speaker"`
	// Name is what the voice is called when downloaded or transcribed,
	// without an extension: the upload's name with the voice's in it.
	Name       string  `json:"name"`
	Duration   float64 `json:"duration"`
	SampleRate int     `json:"sample_rate"`
	// SampleStart is where the sample was taken from when a person placed
	// it; absent when the server chose it.
	SampleStart *float64 `json:"sample_start,omitempty"`
	// JobID is the last transcription submitted from it, if any.
	JobID string `json:"job_id,omitempty"`
}

// IsolationTranscript is where TranscribeSpeakers is. The transcript itself
// is behind IsolationService.Transcript.
type IsolationTranscript struct {
	// Status is queued while the voices are isolated, running while the
	// jobs are, then succeeded or failed.
	Status   JobStatus `json:"status"`
	Model    string    `json:"model,omitempty"`
	Language string    `json:"language,omitempty"`
	// Jobs are the transcriptions merged, one per track.
	Jobs []TranscriptJob `json:"jobs,omitempty"`
	// Stale says a track was isolated again after the transcript was made,
	// so it no longer matches the tracks.
	Stale    bool      `json:"stale,omitempty"`
	Warnings []Warning `json:"warnings,omitempty"`
	Error    *Error    `json:"error,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// TranscriptJob is the transcription of one track.
type TranscriptJob struct {
	Speaker string    `json:"speaker"`
	JobID   string    `json:"job_id"`
	Status  JobStatus `json:"status"`
	Percent int       `json:"percent,omitempty"`
}

// IsolationEvent is one state transition.
type IsolationEvent struct {
	Seq       int64     `json:"seq"`
	Isolation Isolation `json:"isolation"`
}
