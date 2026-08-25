package sherpa

import (
	"context"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/diarize"
)

// Pool hands out diarizers to concurrent jobs, like vad.Pool and for the same
// reason: an instance holds its own onnxruntime sessions, and building one per
// request would add model-load time to every diarized upload.
//
// It deliberately sits outside the model pool. Diarization models are not
// asr.Recognizers, they are not selectable per request, and they are loaded for
// the lifetime of the server — so LRU eviction, the resident-model count and
// max_model_rss_mb do not apply to them. The cost of that choice is that their
// memory is not part of the pool's budget, which is why their manifests carry
// approx_rss_mb and the startup estimate counts them separately.
type Pool struct {
	free chan *Diarizer
	all  []*Diarizer
}

// NewPool builds size diarizers up front, so a bad model path fails at startup
// rather than on the first request that asks for speakers.
func NewPool(cfg diarize.Config, numThreads, size int) (*Pool, error) {
	if size < 1 {
		size = 1
	}
	p := &Pool{free: make(chan *Diarizer, size)}
	for range size {
		d, err := New(cfg, numThreads)
		if err != nil {
			_ = p.Close()
			return nil, err
		}
		p.all = append(p.all, d)
		p.free <- d
	}
	return p, nil
}

// Process waits for a free instance and runs the pass.
//
// The instance is returned only when the C call has come back. A cancelled
// request cannot take its slot with it: releasing early would hand the same
// instance to another job while sherpa-onnx was still inside it.
func (p *Pool) Process(ctx context.Context, pcm audio.PCM, numClusters int) ([]diarize.Turn, error) {
	select {
	case d := <-p.free:
		defer func() { p.free <- d }()
		return d.Process(ctx, pcm, numClusters)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// SampleRate is the rate the models were trained at.
//
// Every instance is built from the same configuration, so asking the first one
// answers for all of them.
func (p *Pool) SampleRate() int {
	if len(p.all) == 0 {
		return 0
	}
	return p.all[0].SampleRate()
}

func (p *Pool) Close() error {
	for _, d := range p.all {
		_ = d.Close()
	}
	p.all = nil
	return nil
}

var _ diarize.Diarizer = (*Pool)(nil)
