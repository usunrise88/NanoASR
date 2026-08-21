package vad

import (
	"context"

	sonnx "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
)

// detector wraps one sherpa-onnx VoiceActivityDetector.
//
// The underlying detector carries per-stream state, so an instance handles one
// recording at a time. Concurrency is served by Pool below rather than by a
// mutex, because serialising VAD would cap the whole pipeline at one job.
type detector struct {
	impl *sonnx.VoiceActivityDetector
	cfg  Config
}

func newDetector(cfg Config) (*detector, error) {
	model := sonnx.VadModelConfig{
		SampleRate: cfg.SampleRate,
		NumThreads: cfg.NumThreads,
		Provider:   "cpu",
	}
	params := struct {
		threshold          float32
		minSilenceDuration float32
		minSpeechDuration  float32
		windowSize         int
		maxSpeechDuration  float32
	}{
		threshold:          cfg.Threshold,
		minSilenceDuration: float32(cfg.MinSilenceMS) / 1000,
		minSpeechDuration:  float32(cfg.MinSpeechMS) / 1000,
		windowSize:         cfg.WindowSize,
		maxSpeechDuration:  cfg.MaxSpeechSec,
	}

	switch cfg.Family {
	case "ten_vad":
		model.TenVad = sonnx.TenVadModelConfig{
			Model:              cfg.ModelPath,
			Threshold:          params.threshold,
			MinSilenceDuration: params.minSilenceDuration,
			MinSpeechDuration:  params.minSpeechDuration,
			WindowSize:         params.windowSize,
			MaxSpeechDuration:  params.maxSpeechDuration,
		}
	default:
		model.SileroVad = sonnx.SileroVadModelConfig{
			Model:              cfg.ModelPath,
			Threshold:          params.threshold,
			MinSilenceDuration: params.minSilenceDuration,
			MinSpeechDuration:  params.minSpeechDuration,
			WindowSize:         params.windowSize,
			MaxSpeechDuration:  params.maxSpeechDuration,
		}
	}

	// The internal buffer must hold the longest segment the detector can emit,
	// with headroom for the tail it accumulates before deciding.
	buffer := cfg.MaxSpeechSec * 2
	if buffer < 30 {
		buffer = 30
	}

	impl := sonnx.NewVoiceActivityDetector(&model, buffer)
	if impl == nil {
		return nil, core.Errorf(core.CodeInternal,
			"sherpa-onnx refused to create a voice activity detector from %s", cfg.ModelPath)
	}
	return &detector{impl: impl, cfg: cfg}, nil
}

func (d *detector) segment(ctx context.Context, pcm audio.PCM) ([]Segment, error) {
	if pcm.SampleRate != d.cfg.SampleRate {
		// The pipeline resamples before this point; reaching here means the
		// normalisation step was skipped, which would silently shift every
		// timestamp we report.
		return nil, core.Errorf(core.CodeInternal,
			"VAD needs %d Hz audio, got %d Hz", d.cfg.SampleRate, pcm.SampleRate)
	}

	// State survives across recordings otherwise, and the first segment of the
	// next file would inherit the tail of this one.
	d.impl.Reset()

	var out []Segment
	window := d.cfg.WindowSize

	for off := 0; off < len(pcm.Samples); off += window {
		// Checked per window rather than per file: a cancelled request should
		// stop within milliseconds, not after ten minutes of audio.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(off+window, len(pcm.Samples))
		d.impl.AcceptWaveform(pcm.Samples[off:end])
		out = d.drain(out)
	}

	// Flush emits whatever speech was still open at the end of the recording.
	d.impl.Flush()
	out = d.drain(out)
	return out, nil
}

// drain moves every completed segment out of the detector. Front copies the
// samples into Go memory, so a popped segment stays valid.
func (d *detector) drain(out []Segment) []Segment {
	for !d.impl.IsEmpty() {
		seg := d.impl.Front()
		d.impl.Pop()
		if seg == nil || len(seg.Samples) == 0 {
			continue
		}
		out = append(out, Segment{StartSample: seg.Start, Samples: seg.Samples})
	}
	return out
}

func (d *detector) close() {
	if d.impl != nil {
		sonnx.DeleteVoiceActivityDetector(d.impl)
		d.impl = nil
	}
}

// Pool hands out detectors to concurrent jobs.
//
// Each detector holds its own onnxruntime session, so the pool is sized to the
// job concurrency rather than created per request: loading Silero for every
// upload would add avoidable latency to every single request.
type Pool struct {
	free chan *detector
	all  []*detector
}

// NewPool creates size detectors up front, so a request never pays model load
// time and a bad model path fails at startup instead of on first use.
func NewPool(cfg Config, size int) (*Pool, error) {
	cfg = cfg.withDefaults()
	if cfg.ModelPath == "" {
		return nil, core.Errorf(core.CodeInvalidRequest, "vad: no model path configured")
	}
	if size < 1 {
		size = 1
	}

	p := &Pool{free: make(chan *detector, size)}
	for range size {
		d, err := newDetector(cfg)
		if err != nil {
			p.Close()
			return nil, err
		}
		p.all = append(p.all, d)
		p.free <- d
	}
	return p, nil
}

func (p *Pool) Segment(ctx context.Context, pcm audio.PCM) ([]Segment, error) {
	select {
	case d := <-p.free:
		defer func() { p.free <- d }()
		return d.segment(ctx, pcm)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *Pool) Close() error {
	for _, d := range p.all {
		d.close()
	}
	p.all = nil
	return nil
}

var _ Segmenter = (*Pool)(nil)
