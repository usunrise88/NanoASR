// Package sherpa is the only place in NanoASR that touches cgo.
//
// Keeping the binding behind asr.Recognizer means the rest of the server is
// testable without models, and a future backend (or an out-of-process worker,
// SPEC §17 M6) is a swap here rather than a rewrite.
package sherpa

import (
	"context"
	"runtime"
	"sync"

	sonnx "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/core"
)

// Versions reports the linked native library versions. Useful in /healthz and
// in bug reports: the Go module version alone does not identify the runtime.
func Versions() (sherpaOnnx, onnxRuntime string) {
	return sonnx.GetVersion(), sonnx.GetOnnxruntimeVersion()
}

// Recognizer wraps one loaded sherpa-onnx offline recogniser.
//
// A single instance is shared by every worker holding a lease on the model:
// onnxruntime sessions are safe for concurrent Run, and duplicating the
// instance would duplicate the weights in RAM for no gain.
type Recognizer struct {
	mu   sync.RWMutex
	impl *sonnx.OfflineRecognizer

	caps         core.Capabilities
	modelingUnit string
	closed       bool
}

// New builds a recogniser from an already-populated sherpa-onnx config. The
// registry fills the config from the model manifest; this package does not read
// manifests so it stays a thin binding.
func New(cfg *sonnx.OfflineRecognizerConfig, caps core.Capabilities, modelingUnit string) (*Recognizer, error) {
	impl := sonnx.NewOfflineRecognizer(cfg)
	if impl == nil {
		return nil, core.Errorf(core.CodeInternal, "sherpa-onnx refused to create the recogniser")
	}
	r := &Recognizer{impl: impl, caps: caps, modelingUnit: modelingUnit}
	// The C++ object owns memory the Go GC cannot see; a finaliser is a
	// backstop, not the contract. Close() is still mandatory.
	runtime.SetFinalizer(r, func(r *Recognizer) { _ = r.Close() })
	return r, nil
}

// Decode runs one batch through DecodeStreams. Batching is what keeps CPU busy:
// per-stream decoding leaves cores idle between short VAD segments.
func (r *Recognizer) Decode(ctx context.Context, batch [][]float32, sampleRate int) ([]asr.Recognition, error) {
	if len(batch) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, core.Errorf(core.CodeModelUnavailable, "recogniser is closed")
	}

	streams := make([]*sonnx.OfflineStream, 0, len(batch))
	defer func() {
		for _, s := range streams {
			sonnx.DeleteOfflineStream(s)
		}
	}()

	for _, samples := range batch {
		s := sonnx.NewOfflineStream(r.impl)
		if s == nil {
			return nil, core.Errorf(core.CodeInternal, "sherpa-onnx refused to create a stream")
		}
		streams = append(streams, s)
		// AcceptWaveform resamples internally when sampleRate differs from
		// what the feature extractor expects, so 8 kHz telephony is accepted
		// directly. The pipeline still normalises to 16 kHz up front because
		// VAD and diarization do not.
		s.AcceptWaveform(sampleRate, samples)
	}

	r.impl.DecodeStreams(streams)

	out := make([]asr.Recognition, len(streams))
	for i, s := range streams {
		res := s.GetResult()
		if res == nil {
			continue
		}
		out[i] = asr.Recognition{
			Text:       res.Text,
			Tokens:     res.Tokens,
			Timestamps: res.Timestamps,
			Durations:  res.Durations,
			LogProbs:   res.YsLogProbs,
			Language:   res.Lang,
		}
	}
	return out, nil
}

func (r *Recognizer) Capabilities() core.Capabilities { return r.caps }
func (r *Recognizer) ModelingUnit() string            { return r.modelingUnit }

// Close releases the native recogniser. It is safe to call more than once, and
// it waits for in-flight Decode calls to finish.
func (r *Recognizer) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	sonnx.DeleteOfflineRecognizer(r.impl)
	r.impl = nil
	runtime.SetFinalizer(r, nil)
	return nil
}

var _ asr.Recognizer = (*Recognizer)(nil)
