package core

import "context"

// RealtimeService opens streaming recognition sessions.
//
// It is separate from Service for the same reason ModelService is: a dialect
// that transcribes uploads has no business holding a decoder open, and a
// deployment with no streaming model configured must still be able to serve
// everything else. A nil RealtimeService means realtime is not available here.
type RealtimeService interface {
	// Open starts a session, or fails because the server is at capacity.
	//
	// It fails rather than queues on purpose: the caller is holding a live
	// microphone, and a session that starts in ninety seconds is worse to them
	// than one that never starts, because by then they have been talking into
	// a socket that was recording nothing.
	Open(ctx context.Context, p RealtimeParams) (RealtimeSession, error)

	// Describe reports what this server will do, for the session
	// acknowledgement a dialect sends before any audio arrives.
	Describe() RealtimeInfo
}

// RealtimeParams is what a client may choose per session. Everything a
// streaming recogniser settles at load time — the model, the decoding method,
// the endpoint rules — is absent, because it cannot be honoured per session
// without a second copy of the model in memory.
type RealtimeParams struct {
	APIKeyID string
	// Language is advisory: the streaming models are monolingual, so this is
	// checked against the model's own languages and reported rather than
	// switching anything.
	Language string
	// ModelID must name the configured streaming model, or be empty. A client
	// asking for another one is told which one is loaded instead of being
	// served by the wrong model in silence.
	ModelID string
	// AutoTurns decides who ends an utterance. True — the default — lets the
	// recogniser end it on trailing silence. False means it ends only when the
	// client commits, which is how push-to-talk works.
	AutoTurns bool
}

// RealtimeInfo describes the loaded streaming model and the session limits.
type RealtimeInfo struct {
	Model      string   `json:"model"`
	Languages  []string `json:"languages,omitempty"`
	SampleRate int      `json:"sample_rate"`
	// Punctuation is whether the model writes sentence marks and capitals.
	// False for every Russian streaming model there is, which clients would
	// otherwise discover by diffing against the offline API.
	Punctuation    bool         `json:"punctuation"`
	WordTimestamps bool         `json:"word_timestamps"`
	AutoTurns      bool         `json:"auto_turns"`
	MaxSessions    int          `json:"max_sessions"`
	Active         int          `json:"active_sessions"`
	Capabilities   Capabilities `json:"capabilities"`
}

// RealtimeSession is one open stream of audio.
//
// Append, Commit, Clear and Close are safe to call from the goroutine reading
// the client's socket. Events is read by whichever goroutine writes to it; the
// channel is closed when the session ends, and the last event before it says
// why.
type RealtimeSession interface {
	// Append queues audio. sampleRate is the rate of these samples.
	//
	// It returns an error when the session has ended, or when more audio is
	// queued than the server is allowed to hold — which means recognition is
	// not keeping up with the stream. Dropping audio silently would produce a
	// transcript with a hole in it and nothing to indicate the loss.
	Append(sampleRate int, samples []float32) error
	// Commit ends the current utterance now, without waiting for silence. The
	// final transcript follows as an event.
	Commit()
	// Clear discards audio that has not been recognised yet and starts a new
	// utterance.
	Clear()
	Events() <-chan RealtimeEvent
	ID() string
	Close() error
}

// RealtimeEventKind is what happened.
type RealtimeEventKind string

const (
	// RealtimeSpeechStarted is the first decoded word of an utterance. It is
	// where speech was noticed, not where it began: the streaming recogniser
	// reports utterance boundaries, not a voice activity decision, so this is
	// the honest approximation of one.
	RealtimeSpeechStarted RealtimeEventKind = "speech_started"
	// RealtimePartial is a revised hypothesis for the current utterance. It is
	// cumulative and it can contradict what came before: a streaming decoder
	// may retract a word.
	RealtimePartial RealtimeEventKind = "partial"
	// RealtimeSpeechStopped is the end of the utterance, by trailing silence
	// or because the client committed.
	RealtimeSpeechStopped RealtimeEventKind = "speech_stopped"
	// RealtimeFinal is the finished transcript of one utterance.
	RealtimeFinal RealtimeEventKind = "final"
	// RealtimeCleared acknowledges Clear.
	RealtimeCleared RealtimeEventKind = "cleared"
	// RealtimeError ends the session; the channel closes after it.
	RealtimeError RealtimeEventKind = "error"
)

// RealtimeEvent is one thing that happened to a session.
type RealtimeEvent struct {
	Kind RealtimeEventKind
	// Item is the id of the utterance this is about. It changes at every
	// boundary, so a client can attach partials to the right line.
	Item string
	// Text is the hypothesis so far, or the final transcript.
	Text string
	// Delta is what Text gained since the last event for this item, empty when
	// the hypothesis was revised rather than extended.
	Delta string
	// StartMS and EndMS are milliseconds of accepted audio, measured from the
	// start of the session. EndMS is only set on the events that end an
	// utterance.
	StartMS int64
	EndMS   int64
	// Words carries token timings when the model produces them and the client
	// asked for them.
	Words []Word
	Err   error
}
