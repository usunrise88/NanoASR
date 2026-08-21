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

// Config mirrors the knobs of SileroVadModelConfig that matter to us.
//
// MaxSpeechSec matters more than it looks: without an upper bound a long
// monologue becomes a single enormous segment, which spikes memory and ruins
// latency on exactly the files this server exists for.
type Config struct {
	Model        string
	Threshold    float32
	MinSilenceMS int
	MinSpeechMS  int
	MaxSpeechSec float32
	SampleRate   int
}

// Segmenter finds speech. Implementations wrap sherpa_onnx.VoiceActivityDetector.
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
		if end := s.StartSample + len(s.Samples); end > cursor {
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
