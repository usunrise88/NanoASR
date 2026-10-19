package tse

import (
	"sync"

	sonnx "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/usunrise88/nanoasr/internal/core"
)

// SherpaEmbedder is an Embedder on sherpa-onnx's speaker embedding extractor:
// CAM++, or any other model sherpa-onnx loads for diarization.
//
// One extractor serves every caller. Compute runs one onnxruntime session,
// which is safe for concurrent Run, on a stream each call owns.
type SherpaEmbedder struct {
	mu sync.RWMutex
	ex *sonnx.SpeakerEmbeddingExtractor
}

// NewSherpaEmbedder loads the model at path.
func NewSherpaEmbedder(path string, threads int) (*SherpaEmbedder, error) {
	ex := sonnx.NewSpeakerEmbeddingExtractor(&sonnx.SpeakerEmbeddingExtractorConfig{
		Model:      path,
		NumThreads: max(1, threads),
		Provider:   "cpu",
	})
	if ex == nil {
		return nil, core.Errorf(core.CodeInternal, "cannot load the speaker embedding model %s", path)
	}
	return &SherpaEmbedder{ex: ex}, nil
}

// Embed returns the unit-length embedding of samples at SampleRate.
func (e *SherpaEmbedder) Embed(samples []float32) ([]float32, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.ex == nil {
		return nil, core.Errorf(core.CodeInternal, "the speaker embedding model is closed")
	}
	s := e.ex.CreateStream()
	defer sonnx.DeleteOnlineStream(s)
	s.AcceptWaveform(SampleRate, samples)
	s.InputFinished()
	if !e.ex.IsReady(s) {
		// Too short for a single frame of features; the gate and the sample
		// selection treat that as no evidence either way.
		return make([]float32, e.ex.Dim()), nil
	}
	return normalise(e.ex.Compute(s)), nil
}

// Close frees the model once nobody is using it.
func (e *SherpaEmbedder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ex != nil {
		sonnx.DeleteSpeakerEmbeddingExtractor(e.ex)
		e.ex = nil
	}
}
