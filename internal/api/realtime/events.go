package realtime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"

	"github.com/usunrise88/nanoasr/internal/core"
)

// The event envelopes of the OpenAI realtime API, transcription intent.
//
// They are written out as structs rather than built from maps so that the
// field names are in one place and a reader can see the whole protocol
// surface. Every server event carries an event_id and a type; clients switch
// on the type and ignore what they do not know, which is what makes the
// nanoasr extension objects safe to send.

// Client event types this server acts on.
const (
	clientSessionUpdate              = "session.update"
	clientTranscriptionSessionUpdate = "transcription_session.update"
	clientAudioAppend                = "input_audio_buffer.append"
	clientAudioCommit                = "input_audio_buffer.commit"
	clientAudioClear                 = "input_audio_buffer.clear"
)

// Server event types this server sends.
const (
	serverSessionCreated = "transcription_session.created"
	serverSessionUpdated = "transcription_session.updated"
	serverSpeechStarted  = "input_audio_buffer.speech_started"
	serverSpeechStopped  = "input_audio_buffer.speech_stopped"
	serverCommitted      = "input_audio_buffer.committed"
	serverCleared        = "input_audio_buffer.cleared"
	serverItemCreated    = "conversation.item.created"
	serverDelta          = "conversation.item.input_audio_transcription.delta"
	serverCompleted      = "conversation.item.input_audio_transcription.completed"
	serverFailed         = "conversation.item.input_audio_transcription.failed"
	serverError          = "error"
	// serverWarning is NanoASR's own. The protocol has no way to say "that
	// field was accepted and ignored", and silently dropping a parameter a
	// client set deliberately is how people end up debugging the wrong thing.
	serverWarning = "nanoasr.warning"
)

// clientEvent is the envelope every client message arrives in.
type clientEvent struct {
	Type    string `json:"type"`
	EventID string `json:"event_id,omitempty"`
	// Audio is base64 for input_audio_buffer.append.
	Audio string `json:"audio,omitempty"`
	// Session is the body of a session update. Raw because the two spellings
	// of that event carry slightly different shapes and because a field we do
	// not implement must be reportable rather than silently dropped.
	Session json.RawMessage `json:"session,omitempty"`
}

// sessionPatch is what a client may ask to change.
type sessionPatch struct {
	Type string `json:"type"`
	// InputAudioFormat is "pcm16", "g711_ulaw", "g711_alaw", or the object
	// form {"type": "audio/pcm", "rate": 16000}.
	InputAudioFormat json.RawMessage `json:"input_audio_format"`
	Audio            *struct {
		Input *struct {
			Format json.RawMessage `json:"format"`
		} `json:"input"`
	} `json:"audio"`
	InputAudioTranscription *transcriptionPatch `json:"input_audio_transcription"`
	// TurnDetection is null to switch automatic turns off, or an object to
	// configure them. Raw so that null is distinguishable from absent: one
	// means "I will commit the buffer myself" and the other means "leave it
	// as it is".
	TurnDetection json.RawMessage `json:"turn_detection"`

	// Accepted and ignored. Named explicitly so that the warning can say
	// which field was dropped instead of a generic complaint.
	Modalities         []string        `json:"modalities"`
	Include            []string        `json:"include"`
	InputAudioNoise    json.RawMessage `json:"input_audio_noise_reduction"`
	Instructions       string          `json:"instructions"`
	Temperature        *float64        `json:"temperature"`
	ClientSecret       json.RawMessage `json:"client_secret"`
	Voice              string          `json:"voice"`
	OutputAudioFormat  json.RawMessage `json:"output_audio_format"`
	MaxResponseOutput  json.RawMessage `json:"max_response_output_tokens"`
	Tools              json.RawMessage `json:"tools"`
	ToolChoice         json.RawMessage `json:"tool_choice"`
	TranscriptionTurns json.RawMessage `json:"turn_detection_mode"`
}

type transcriptionPatch struct {
	Model    string `json:"model"`
	Language string `json:"language"`
	Prompt   string `json:"prompt"`
}

// sessionObject is what created and updated carry back.
type sessionObject struct {
	ID                      string              `json:"id"`
	Object                  string              `json:"object"`
	Model                   string              `json:"model"`
	InputAudioFormat        string              `json:"input_audio_format"`
	InputAudioTranscription *transcriptionPatch `json:"input_audio_transcription"`
	TurnDetection           *turnDetection      `json:"turn_detection"`
	NanoASR                 sessionExtra        `json:"nanoasr"`
}

type turnDetection struct {
	Type string `json:"type"`
	// SilenceDurationMS is the trailing silence that ends an utterance. It is
	// reported, not accepted: sherpa-onnx settles it when the model is loaded.
	SilenceDurationMS int `json:"silence_duration_ms,omitempty"`
}

// sessionExtra is everything a caller needs that OpenAI's shape has no room
// for. All of it is measured from the loaded model rather than configured.
type sessionExtra struct {
	ModelSampleRate int      `json:"model_sample_rate"`
	InputSampleRate int      `json:"input_sample_rate"`
	Languages       []string `json:"languages,omitempty"`
	Punctuation     bool     `json:"punctuation"`
	WordTimestamps  bool     `json:"word_timestamps"`
	MaxSessions     int      `json:"max_sessions"`
	ActiveSessions  int      `json:"active_sessions"`
	Notes           []string `json:"notes,omitempty"`
}

type eventSessionState struct {
	Type    string        `json:"type"`
	EventID string        `json:"event_id"`
	Session sessionObject `json:"session"`
}

type eventSpeech struct {
	Type         string `json:"type"`
	EventID      string `json:"event_id"`
	ItemID       string `json:"item_id"`
	AudioStartMS *int64 `json:"audio_start_ms,omitempty"`
	AudioEndMS   *int64 `json:"audio_end_ms,omitempty"`
}

type eventCommitted struct {
	Type           string `json:"type"`
	EventID        string `json:"event_id"`
	ItemID         string `json:"item_id"`
	PreviousItemID string `json:"previous_item_id,omitempty"`
}

type eventCleared struct {
	Type    string `json:"type"`
	EventID string `json:"event_id"`
}

type eventItemCreated struct {
	Type           string `json:"type"`
	EventID        string `json:"event_id"`
	PreviousItemID string `json:"previous_item_id,omitempty"`
	Item           item   `json:"item"`
}

type item struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Type    string        `json:"type"`
	Role    string        `json:"role"`
	Status  string        `json:"status"`
	Content []itemContent `json:"content"`
}

type itemContent struct {
	Type       string  `json:"type"`
	Transcript *string `json:"transcript"`
}

type eventDelta struct {
	Type         string      `json:"type"`
	EventID      string      `json:"event_id"`
	ItemID       string      `json:"item_id"`
	ContentIndex int         `json:"content_index"`
	Delta        string      `json:"delta"`
	NanoASR      *deltaExtra `json:"nanoasr,omitempty"`
}

// deltaExtra carries the whole hypothesis beside the increment.
//
// A streaming decoder revises as well as extends: when it changes its mind
// about a word already sent, there is no delta that expresses it, and a client
// appending deltas would be left with text the recogniser has retracted. The
// full text is the authoritative value, and this is the only place to put it.
type deltaExtra struct {
	Text    string `json:"text"`
	StartMS int64  `json:"start_ms"`
}

type eventCompleted struct {
	Type         string          `json:"type"`
	EventID      string          `json:"event_id"`
	ItemID       string          `json:"item_id"`
	ContentIndex int             `json:"content_index"`
	Transcript   string          `json:"transcript"`
	NanoASR      *completedExtra `json:"nanoasr,omitempty"`
}

type completedExtra struct {
	StartMS int64       `json:"start_ms"`
	EndMS   int64       `json:"end_ms"`
	Model   string      `json:"model"`
	Words   []core.Word `json:"words,omitempty"`
}

type eventError struct {
	Type    string    `json:"type"`
	EventID string    `json:"event_id"`
	Error   errorBody `json:"error"`
}

type errorBody struct {
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	Param   string `json:"param,omitempty"`
	// EventID is the client event this is about, when it is about one.
	EventID string `json:"event_id,omitempty"`
}

type eventWarning struct {
	Type    string `json:"type"`
	EventID string `json:"event_id"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param,omitempty"`
}

// newEventID is the id every server event carries.
func newEventID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "event_" + hex.EncodeToString(b[:])
}

func ptr[T any](v T) *T { return &v }
