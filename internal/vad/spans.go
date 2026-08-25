package vad

import (
	"sort"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Span is a half-open sample range: speech extents without the speech.
//
// It exists for channel split. Silence and speech-ratio arithmetic needs to
// combine what several channels found, and a Segment carries its own samples —
// merging segments would mean copying the audio a second time to answer a
// question about time. Two legs of a call talking at once is one span, and the
// silence a player paints is where every leg is quiet.
type Span struct {
	Start int
	End   int
}

// Spans projects segments onto their extents.
func Spans(segs []Segment) []Span {
	out := make([]Span, 0, len(segs))
	for _, s := range segs {
		out = append(out, Span{Start: s.StartSample, End: s.EndSample()})
	}
	return out
}

// MergeSpans sorts every set together and coalesces the ones that touch or
// overlap, so the result is disjoint and in order.
func MergeSpans(sets ...[]Span) []Span {
	var all []Span
	for _, s := range sets {
		all = append(all, s...)
	}
	if len(all) < 2 {
		return all
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Start != all[j].Start {
			return all[i].Start < all[j].Start
		}
		return all[i].End < all[j].End
	})

	out := all[:1]
	for _, s := range all[1:] {
		last := &out[len(out)-1]
		if s.Start <= last.End {
			if s.End > last.End {
				last.End = s.End
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

// SilencesFrom is Silences over pre-merged spans.
func SilencesFrom(spans []Span, totalSamples, sampleRate, minSilenceMS int) []core.Silence {
	if sampleRate <= 0 || totalSamples <= 0 {
		return nil
	}
	minSamples := minSilenceMS * sampleRate / 1000
	toSec := func(n int) float64 { return float64(n) / float64(sampleRate) }

	var out []core.Silence
	cursor := 0
	for _, s := range spans {
		if gap := s.Start - cursor; gap >= minSamples {
			out = append(out, core.Silence{Start: toSec(cursor), End: toSec(s.Start)})
		}
		if s.End > cursor {
			cursor = s.End
		}
	}
	if gap := totalSamples - cursor; gap >= minSamples {
		out = append(out, core.Silence{Start: toSec(cursor), End: toSec(totalSamples)})
	}
	return out
}

// SpeechRatioFrom is SpeechRatio over pre-merged spans. Spans must be disjoint
// — otherwise two channels speaking at once would count their overlap twice
// and the ratio could exceed 1.
func SpeechRatioFrom(spans []Span, totalSamples int) float64 {
	if totalSamples <= 0 {
		return 0
	}
	speech := 0
	for _, s := range spans {
		speech += s.End - s.Start
	}
	return float64(speech) / float64(totalSamples)
}
