package vad

import (
	"context"
	"fmt"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/coreml"
)

// FamilyCoreMLSilero is the catalog's Silero VAD converted for CoreML: the
// network runs on the Neural Engine, many 32 ms windows per call, and the
// speech/silence decisions are made here, by a port of sherpa-onnx's detector.
//
// The network is the one silero-vad-v5 actually ships — k2-fsa's 16 kHz export,
// which its own metadata calls Silero v4 — rebuilt from that file's weights, so
// that switching to CoreML moves no segment boundary.
const FamilyCoreMLSilero = "coreml_silero_vad"

// Silero v4 at 16 kHz, as sherpa-onnx drives it: 512-sample windows, back to
// back, each reflect-padded by 96 on both sides and cut into 8 STFT frames of
// 256 at a hop of 64.
const (
	sileroWindow = 512
	sileroShift  = sileroWindow
	sileroPad    = 96
	sileroLayers = 2
	sileroHidden = 64
	sileroFrames = 8   // STFT frames per window
	sileroFrame  = 256 // STFT frame length
	sileroHop    = 64  // STFT hop
	// sileroScale is what the converted network expects the audio multiplied
	// by: it keeps the spectrum of quiet audio inside fp16's range.
	sileroScale = 256
)

// CoreMLSegmenter is Silero VAD on the Neural Engine.
//
// The probability of each window depends only on the audio and the LSTM state
// before it, never on the decisions, so a recording's probabilities are
// computed first, many windows per prediction, and the decisions after. That
// order is what lets the network run in batches of a hundred windows instead
// of one call per 32 ms.
type CoreMLSegmenter struct {
	cfg   Config
	m     *coreml.Model
	batch int // windows per prediction
}

// NewCoreML loads the converted detector from a compiled model directory.
func NewCoreML(cfg Config, modelPath string) (*CoreMLSegmenter, error) {
	cfg = cfg.withDefaults()
	if cfg.SampleRate != 16000 {
		return nil, core.Errorf(core.CodeInvalidRequest,
			"vad: the CoreML Silero detector runs at 16000 Hz, configured for %d", cfg.SampleRate)
	}
	m, err := coreml.Load(modelPath, "", coreml.NeuralEngine)
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "vad: %s", err.Error()).WithCause(err)
	}
	in, err := m.Description().Input("frames")
	if err != nil || len(in.Shape) != 3 || in.Shape[1] != sileroFrames || in.Shape[2] != sileroFrame {
		m.Close()
		return nil, core.Errorf(core.CodeInternal, "vad: %s is not the converted Silero detector", modelPath)
	}
	return &CoreMLSegmenter{cfg: cfg, m: m, batch: in.Shape[0]}, nil
}

// Segment finds the speech in one recording.
func (s *CoreMLSegmenter) Segment(ctx context.Context, pcm audio.PCM) ([]Segment, error) {
	if pcm.SampleRate != s.cfg.SampleRate {
		return nil, core.Errorf(core.CodeInternal,
			"VAD needs %d Hz audio, got %d Hz", s.cfg.SampleRate, pcm.SampleRate)
	}
	probs, err := s.probabilities(ctx, pcm.Samples)
	if err != nil {
		return nil, err
	}
	return decide(s.cfg, probs, pcm.Samples), nil
}

// probabilities runs the network over every window sherpa-onnx would have
// run it over: those that end inside the recording.
func (s *CoreMLSegmenter) probabilities(ctx context.Context, x []float32) ([]float32, error) {
	if len(x) < sileroWindow {
		return nil, nil
	}
	n := (len(x)-sileroWindow)/sileroShift + 1
	out := make([]float32, 0, n)
	frames := make([]float32, s.batch*sileroFrames*sileroFrame)
	h := make([]float32, sileroLayers*sileroHidden)
	c := make([]float32, sileroLayers*sileroHidden)
	prob := make([]float32, s.batch)
	hOut := make([]float32, len(h))
	cOut := make([]float32, len(c))
	var padded [sileroWindow + 2*sileroPad]float32
	for first := 0; first < n; first += s.batch {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		clear(frames)
		count := min(s.batch, n-first)
		for w := range count {
			// Silero's own padding: the window reflected on both sides.
			win := x[(first+w)*sileroShift : (first+w)*sileroShift+sileroWindow]
			for i := range sileroPad {
				padded[sileroPad-1-i] = win[i+1]
				padded[sileroPad+sileroWindow+i] = win[sileroWindow-2-i]
			}
			copy(padded[sileroPad:], win)
			for i := range padded {
				padded[i] *= sileroScale
			}
			dst := frames[w*sileroFrames*sileroFrame:]
			for f := range sileroFrames {
				copy(dst[f*sileroFrame:(f+1)*sileroFrame], padded[f*sileroHop:])
			}
		}
		err := s.m.Predict(
			map[string][]float32{"frames": frames, "h": h, "c": c},
			map[string][]float32{"prob": prob, "h_out": hOut, "c_out": cOut})
		if err != nil {
			return nil, core.Errorf(core.CodeInternal, "vad: %s", err.Error()).WithCause(err)
		}
		out = append(out, prob[:count]...)
		// The state after the padding windows of a final short batch is never
		// read again, so carrying it over unconditionally is safe.
		copy(h, hOut)
		copy(c, cOut)
	}
	return out, nil
}

// decide is sherpa-onnx's VoiceActivityDetector over precomputed window
// probabilities: SileroVadModel::IsSpeech for the hysteresis, and the
// detector's AcceptWaveform for turning decisions into segments, including the
// way it forces a cut once an utterance outgrows max_speech_duration — a
// threshold of 0.9 and 100 ms of silence until one comes.
//
// Positions are absolute sample indices; head and tail are the ends of the
// detector's buffer.
func decide(cfg Config, probs []float32, x []float32) []Segment {
	const (
		forcedThreshold = 0.9
		forcedSilenceMS = 100
	)
	rate := cfg.SampleRate
	minSpeech := rate * cfg.MinSpeechMS / 1000
	maxUtterance := int(float32(rate) * cfg.MaxSpeechSec)

	var (
		out                   []Segment
		head, tail            int
		start                 = -1
		triggered             bool
		current               int
		tempStart, tempEnd    int
		threshold, minSilence float32
	)
	emit := func(from, to int) {
		if to > from {
			out = append(out, Segment{StartSample: from, Samples: x[from:to]})
		}
	}
	isSpeech := func(p float32) bool {
		current += sileroShift
		if p > threshold && tempEnd != 0 {
			tempEnd = 0
		}
		if p > threshold && tempStart == 0 {
			tempStart = current
			return false
		}
		if p > threshold && tempStart != 0 && !triggered {
			if current-tempStart < minSpeech {
				return false
			}
			triggered = true
			return true
		}
		if p < threshold && !triggered {
			tempStart, tempEnd = 0, 0
			return false
		}
		if p > max(threshold-0.15, 0.01) && triggered {
			return true
		}
		if p > threshold && !triggered {
			triggered = true
			return true
		}
		if p < threshold && triggered {
			if tempEnd == 0 {
				tempEnd = current
			}
			if float32(current-tempEnd) < minSilence {
				return true
			}
			tempStart, tempEnd = 0, 0
			triggered = false
			return false
		}
		return false
	}

	for _, p := range probs {
		if tail-head > maxUtterance {
			threshold, minSilence = forcedThreshold, float32(rate*forcedSilenceMS/1000)
		} else {
			threshold, minSilence = cfg.Threshold, float32(rate*cfg.MinSilenceMS/1000)
		}
		tail += sileroShift
		if isSpeech(p) {
			if start == -1 {
				start = max(tail-2*sileroWindow-minSpeech, head)
			}
			continue
		}
		if start != -1 && tail > head {
			end := tail - int(minSilence)
			emit(start, end)
			head = max(head, end)
		}
		if start == -1 {
			if end := tail - 2*sileroWindow - minSpeech; end > head {
				head = end
			}
		}
		start = -1
	}
	// Flush: speech still open when the recording ends.
	if start != -1 && tail > head && tail > start {
		emit(start, min(tail, len(x)))
	}
	return out
}

func (s *CoreMLSegmenter) Close() error {
	if s.m == nil {
		return nil
	}
	err := s.m.Close()
	s.m = nil
	return err
}

var _ Segmenter = (*CoreMLSegmenter)(nil)

// String names the detector for the startup log.
func (s *CoreMLSegmenter) String() string {
	return fmt.Sprintf("silero v5 on CoreML, %d windows per call", s.batch)
}
