package vad

import (
	"context"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
)

// Silero wraps sherpa_onnx.VoiceActivityDetector. It requires 16 kHz input;
// the pipeline guarantees that before calling.
type Silero struct {
	cfg Config
}

func NewSilero(cfg Config) (*Silero, error) { return &Silero{cfg: cfg}, nil }

// TODO(M1): feed pcm through AcceptWaveform in windows, drain with Front/Pop,
// call Flush at the end, and check ctx between windows so cancellation lands
// within a window rather than at the end of the file.
func (s *Silero) Segment(context.Context, audio.PCM) ([]Segment, error) {
	return nil, core.ErrNotImplemented
}

func (s *Silero) Close() error { return nil }

var _ Segmenter = (*Silero)(nil)
