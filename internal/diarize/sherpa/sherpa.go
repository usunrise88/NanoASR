// Package sherpa binds internal/diarize to sherpa-onnx.
//
// It is a separate package from internal/diarize for the same reason
// internal/asr/sherpa is separate from internal/asr: everything above it —
// turn assignment, segment splitting, the speaker summary — is arithmetic over
// times, and keeping cgo out of that half means it can be tested without any
// models on disk.
package sherpa

import (
	"context"
	"runtime"
	"sync"

	sonnx "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/diarize"
)

// Diarizer is one sherpa-onnx speaker diarization instance.
//
// It carries a mutex because Process is one long C++ call over shared state and
// the binding makes no concurrency promise. Concurrency comes from the pool
// below, which holds several instances, rather than from sharing one.
type Diarizer struct {
	mu   sync.Mutex
	impl *sonnx.OfflineSpeakerDiarization
	// clusters is what the instance is currently configured for, so a run of
	// requests asking for the same speaker count reconfigures nothing.
	clusters  int
	threshold float32
	closed    bool
}

// New builds a diarizer from the two models it needs.
func New(cfg diarize.Config, numThreads int) (*Diarizer, error) {
	if cfg.SegmentationModel == "" || cfg.EmbeddingModel == "" {
		return nil, core.Errorf(core.CodeInvalidRequest,
			"diarization needs both a segmentation and an embedding model")
	}
	if numThreads < 1 {
		numThreads = 1
	}

	c := sonnx.OfflineSpeakerDiarizationConfig{
		Segmentation: sonnx.OfflineSpeakerSegmentationModelConfig{
			Pyannote:   sonnx.OfflineSpeakerSegmentationPyannoteModelConfig{Model: cfg.SegmentationModel},
			NumThreads: numThreads,
			Provider:   "cpu",
		},
		Embedding: sonnx.SpeakerEmbeddingExtractorConfig{
			Model:      cfg.EmbeddingModel,
			NumThreads: numThreads,
			Provider:   "cpu",
		},
		Clustering: sonnx.FastClusteringConfig{
			NumClusters: cfg.NumClusters,
			Threshold:   cfg.Threshold,
		},
		MinDurationOn:  cfg.MinDurationOn,
		MinDurationOff: cfg.MinDurationOff,
	}

	impl := sonnx.NewOfflineSpeakerDiarization(&c)
	if impl == nil {
		return nil, core.Errorf(core.CodeInternal,
			"sherpa-onnx refused to create the diarizer; check the segmentation and embedding models")
	}
	d := &Diarizer{impl: impl, clusters: cfg.NumClusters, threshold: cfg.Threshold}
	// The C++ object owns memory the Go GC cannot see. Close is the contract;
	// this is only a backstop.
	runtime.SetFinalizer(d, func(d *Diarizer) { _ = d.Close() })
	return d, nil
}

// SampleRate is the rate the models were trained at, so a caller can check
// rather than assume.
func (d *Diarizer) SampleRate() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return 0
	}
	return d.impl.SampleRate()
}

// Process runs the second pass over the whole recording.
//
// Cancellation is checked before the call and not during it, and that is a
// fixed limitation rather than an oversight. The C API has
// SherpaOnnxOfflineSpeakerDiarizationProcessWithCallback, which would allow
// both progress and cancellation; the Go binding does not export it. So a
// cancelled job can outlive its cancellation by the length of one diarization
// pass — roughly 0.3-0.5x the audio duration.
func (d *Diarizer) Process(ctx context.Context, pcm audio.PCM, numClusters int) ([]diarize.Turn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The binding indexes &samples[0] unconditionally, so an empty recording
	// is a panic rather than an empty result.
	if len(pcm.Samples) == 0 {
		return nil, nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, core.Errorf(core.CodeInternal, "diarizer is closed")
	}

	// A caller who knows how many people are on the call gets a much better
	// answer than threshold clustering. SetConfig applies only the clustering
	// block — the binding says so — and the lock plus the pool's exclusive
	// lease are what make mutating a shared instance safe here.
	if numClusters != d.clusters {
		d.setClustersLocked(numClusters)
	}

	segments := d.impl.Process(pcm.Samples)
	if len(segments) == 0 {
		return nil, nil
	}

	turns := make([]diarize.Turn, 0, len(segments))
	for _, s := range segments {
		turns = append(turns, diarize.Turn{
			Start:   float64(s.Start),
			End:     float64(s.End),
			Speaker: s.Speaker,
		})
	}
	return turns, nil
}

func (d *Diarizer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	sonnx.DeleteOfflineSpeakerDiarization(d.impl)
	d.impl = nil
	runtime.SetFinalizer(d, nil)
	return nil
}

var _ diarize.Diarizer = (*Diarizer)(nil)

// setClustersLocked reconfigures the clustering for one request. The caller
// holds d.mu.
func (d *Diarizer) setClustersLocked(numClusters int) {
	d.impl.SetConfig(&sonnx.OfflineSpeakerDiarizationConfig{
		Clustering: sonnx.FastClusteringConfig{
			NumClusters: numClusters,
			Threshold:   d.threshold,
		},
	})
	d.clusters = numClusters
}
