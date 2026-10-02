package asr

import "github.com/usunrise88/nanoasr/internal/core"

// Streaming is a loaded streaming recogniser: one set of weights, shared by
// every live connection, plus a factory for the per-connection state.
//
// It is a separate interface from Recognizer rather than an option on it
// because the two have almost nothing in common. A Recognizer is handed a
// finished segment and returns a transcript; a Streaming recogniser is fed
// audio as it arrives, is asked repeatedly whether it has enough to decode,
// and reports a hypothesis that keeps changing until it decides the utterance
// has ended. The models are different exports too — a streaming model is
// trained with limited right context — so no set of weights implements both.
type Streaming interface {
	// NewStream starts one independent recognition stream.
	NewStream() (Stream, error)

	// Decode advances every stream in the batch by one step. The caller must
	// have checked Ready on each; sherpa-onnx is explicit that decoding a
	// stream that is not ready is undefined.
	//
	// It takes a batch because that is what makes the arithmetic worth doing
	// on a GPU: one 16 kHz stream asks for a few milliseconds of work every
	// hundred milliseconds, which no accelerator can be kept busy with.
	Decode(streams []Stream)

	// SampleRate is what the model's feature extractor expects. Audio at any
	// other rate is resampled on the way in, so this is informational — except
	// that it is also what the transcript's timestamps are measured in.
	SampleRate() int
	ModelID() string
	ModelingUnit() string
	Capabilities() core.Capabilities
	Close() error
}

// Stream is the decoder state of one connection.
//
// Not safe for concurrent use, and not merely by omission: the object behind it
// holds the feature extractor's buffer and the decoder's state, and Accept and
// Decode both write to them. Every method on one Stream, including the Decode
// batch it is passed to, must be called from the single goroutine that owns it.
type Stream interface {
	// Accept adds audio. sampleRate is the rate of these samples, resampled
	// internally when it differs from the model's.
	Accept(sampleRate int, samples []float32)
	// Ready reports whether enough frames have accumulated to decode.
	Ready() bool
	// Hypothesis is the transcript of the current utterance so far. It is
	// cumulative and it can be revised downwards: a streaming decoder may
	// retract a word it had emitted.
	Hypothesis() Hypothesis
	// Endpoint reports whether the recogniser has decided the utterance ended.
	Endpoint() bool
	// Reset ends the current utterance and starts the next one. The audio
	// already accepted but not yet decoded is kept.
	Reset()
	// Finish declares the audio over so the tail is flushed. Accept must not
	// be called afterwards.
	Finish()
	Close()
}

// Hypothesis is one streaming result.
type Hypothesis struct {
	Text   string
	Tokens []string
	// Timestamps are seconds from the start of the current utterance, one per
	// token, and empty when the model does not produce them.
	Timestamps []float32
}
