// Package asr is the boundary between NanoASR and sherpa-onnx.
//
// Everything above this package works with Recognition, which is model-family
// agnostic. Adding a family means adding one file in families/ (SPEC §7.2) —
// no pipeline, registry or API change.
package asr

import (
	"context"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Recognition is the raw output of one decoded segment, before words are
// assembled. Timestamps and Durations are seconds relative to the segment.
type Recognition struct {
	Text       string
	Tokens     []string
	Timestamps []float32
	Durations  []float32
	LogProbs   []float32
	Language   string
}

// HasTimestamps reports whether this result can carry word-level timings.
func (r Recognition) HasTimestamps() bool {
	return len(r.Timestamps) > 0 && len(r.Timestamps) == len(r.Tokens)
}

// Recognizer decodes prepared audio segments. Implementations are safe for
// concurrent use: onnxruntime sessions are, and a single loaded model is shared
// by every worker holding a lease on it.
type Recognizer interface {
	// Decode takes a batch of segments and returns one Recognition each, in
	// order. Batching is what keeps CPU utilisation high on long files.
	Decode(ctx context.Context, batch [][]float32, sampleRate int) ([]Recognition, error)
	Capabilities() core.Capabilities
	// ModelingUnit tells the word assembler how to group tokens.
	ModelingUnit() string
	Close() error
}
