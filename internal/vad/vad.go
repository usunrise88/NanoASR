// Package vad splits a long recording into speech segments and reports the
// silence between them.
//
// Two reasons this is not optional for the primary workload: offline models
// degrade on multi-minute inputs, and the UI player needs the silence map.
package vad

import (
	"context"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
)

// Segment is one stretch of detected speech, in samples relative to the start
// of the recording.
type Segment struct {
	StartSample int
	Samples     []float32
}

// EndSample is one past the last sample of the segment.
func (s Segment) EndSample() int { return s.StartSample + len(s.Samples) }

// Config describes the detector.
//
// MaxSpeechSec matters more than it looks: without an upper bound a long
// monologue becomes a single enormous segment, which spikes memory and ruins
// latency on exactly the files this server exists for.
type Config struct {
	// Family selects the detector: silero_vad or ten_vad.
	Family    string
	ModelPath string

	Threshold    float32
	MinSilenceMS int
	MinSpeechMS  int
	MaxSpeechSec float32
	SampleRate   int
	NumThreads   int
	// WindowSize is the detector's frame size in samples. 512 is what Silero
	// v5 expects at 16 kHz; a mismatch silently degrades detection.
	WindowSize int
}

func (c Config) withDefaults() Config {
	if c.Family == "" {
		c.Family = "silero_vad"
	}
	if c.SampleRate <= 0 {
		c.SampleRate = 16000
	}
	if c.Threshold <= 0 {
		c.Threshold = 0.5
	}
	if c.MinSilenceMS <= 0 {
		c.MinSilenceMS = 300
	}
	if c.MinSpeechMS <= 0 {
		c.MinSpeechMS = 250
	}
	if c.MaxSpeechSec <= 0 {
		c.MaxSpeechSec = 20
	}
	if c.NumThreads <= 0 {
		c.NumThreads = 1
	}
	if c.WindowSize <= 0 {
		c.WindowSize = 512
	}
	return c
}

// Segmenter finds speech.
type Segmenter interface {
	Segment(ctx context.Context, pcm audio.PCM) ([]Segment, error)
	Close() error
}

// Silences returns the complement of segs over [0, totalSamples), keeping only
// gaps at least minSilenceMS long. This is what the player paints and what the
// API returns as silence[].
func Silences(segs []Segment, totalSamples, sampleRate, minSilenceMS int) []core.Silence {
	if sampleRate <= 0 || totalSamples <= 0 {
		return nil
	}
	minSamples := minSilenceMS * sampleRate / 1000
	toSec := func(n int) float64 { return float64(n) / float64(sampleRate) }

	var out []core.Silence
	cursor := 0
	for _, s := range segs {
		if gap := s.StartSample - cursor; gap >= minSamples {
			out = append(out, core.Silence{Start: toSec(cursor), End: toSec(s.StartSample)})
		}
		if end := s.EndSample(); end > cursor {
			cursor = end
		}
	}
	if gap := totalSamples - cursor; gap >= minSamples {
		out = append(out, core.Silence{Start: toSec(cursor), End: toSec(totalSamples)})
	}
	return out
}

// SpeechRatio is the fraction of the recording that is speech. On telephony it
// usually lands between 0.5 and 0.8, and it is what actually predicts
// processing time — not the file length.
func SpeechRatio(segs []Segment, totalSamples int) float64 {
	if totalSamples <= 0 {
		return 0
	}
	speech := 0
	for _, s := range segs {
		speech += len(s.Samples)
	}
	return float64(speech) / float64(totalSamples)
}

// Whole returns a single segment covering the entire recording, used when VAD
// is disabled.
func Whole(pcm audio.PCM) []Segment {
	return []Segment{{StartSample: 0, Samples: pcm.Samples}}
}

// Disabled is a Segmenter that treats the whole recording as one utterance.
type Disabled struct{}

func (Disabled) Segment(_ context.Context, pcm audio.PCM) ([]Segment, error) {
	return Whole(pcm), nil
}

func (Disabled) Close() error { return nil }

var _ Segmenter = Disabled{}
