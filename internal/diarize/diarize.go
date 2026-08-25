// Package diarize answers "who spoke when" as an opt-in second pass.
//
// It is a request option rather than a pipeline default because it costs
// roughly another 0.3–0.5x RTF on top of recognition, and the primary workload
// is single-speaker (SPEC §5.7).
package diarize

import (
	"context"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
)

// Turn is one contiguous stretch attributed to one speaker cluster.
type Turn struct {
	Start   float64
	End     float64
	Speaker int
}

// Config names the two extra models this needs and how to cluster.
type Config struct {
	SegmentationModel string
	EmbeddingModel    string
	// NumClusters is used when the number of speakers is known; otherwise
	// clustering falls back to Threshold.
	NumClusters    int
	Threshold      float32
	MinDurationOn  float32
	MinDurationOff float32
}

// Diarizer wraps sherpa_onnx.OfflineSpeakerDiarization.
//
// numClusters is the request's speaker count, or 0 to cluster by threshold.
// It is a parameter rather than configuration because it is the one clustering
// knob a caller may set per request, and an implementation that has to mutate
// shared state to honour it can then do so while it holds the instance.
type Diarizer interface {
	Process(ctx context.Context, pcm audio.PCM, numClusters int) ([]Turn, error)
	Close() error
}

// Attribution is one word's speaker and how sure we are it is that speaker
// rather than the neighbour's.
//
// Confidence is the fraction of the word the winning turn actually covered, so
// a word wholly inside one turn scores 1 and a word straddling a boundary
// scores the larger share. It is not a probability and does not pretend to be
// one; it exists so a client can tell a measured attribution from a guessed
// one, and so a UI can mark the guesses.
type Attribution struct {
	Speaker    string
	Confidence float64
}

// FallbackConfidence is what a word gets when no turn overlapped it at all and
// the nearest one was used instead. SPEC §5.7 requires the drop; the value is
// arbitrary but deliberately low enough to sort to the bottom.
const FallbackConfidence = 0.3

// Assign attributes each word to the speaker whose turn overlaps it most.
//
// Words that overlap nothing — a word in a gap in the turn map, or before the
// first turn — fall back to the nearest turn at FallbackConfidence. The
// fallback used to be invisible to the caller, which made the confidence drop
// SPEC §5.7 asks for impossible to produce.
func Assign(words []core.Word, turns []Turn) []Attribution {
	out := make([]Attribution, len(words))
	for i, w := range words {
		best, bestOverlap := -1, 0.0
		for _, t := range turns {
			ov := overlap(w.Start, w.End, t.Start, t.End)
			if ov > bestOverlap {
				best, bestOverlap = t.Speaker, ov
			}
		}
		if best >= 0 {
			out[i] = Attribution{
				Speaker:    speakerID(best),
				Confidence: share(bestOverlap, w.End-w.Start),
			}
			continue
		}
		if best = nearest(w, turns); best >= 0 {
			out[i] = Attribution{Speaker: speakerID(best), Confidence: FallbackConfidence}
		}
	}
	return out
}

// share is overlap as a fraction of the word, clamped. A zero-length word — a
// model that reported identical start and end — would divide by zero, and the
// honest answer for it is the fallback rather than either 0 or 1.
func share(overlap, duration float64) float64 {
	if duration <= 0 {
		return FallbackConfidence
	}
	if r := overlap / duration; r < 1 {
		return r
	}
	return 1
}

func overlap(aStart, aEnd, bStart, bEnd float64) float64 {
	lo := max(aStart, bStart)
	hi := min(aEnd, bEnd)
	if hi <= lo {
		return 0
	}
	return hi - lo
}

func nearest(w core.Word, turns []Turn) int {
	best, bestDist := -1, 0.0
	mid := (w.Start + w.End) / 2
	for _, t := range turns {
		d := 0.0
		switch {
		case mid < t.Start:
			d = t.Start - mid
		case mid > t.End:
			d = mid - t.End
		}
		if best < 0 || d < bestDist {
			best, bestDist = t.Speaker, d
		}
	}
	return best
}

func speakerID(n int) string {
	return "spk_" + itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
