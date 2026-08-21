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
type Diarizer interface {
	Process(ctx context.Context, pcm audio.PCM) ([]Turn, error)
	Close() error
}

// Assign attributes each word to the speaker whose turn overlaps it most.
//
// Words that overlap nothing (a word straddling a turn boundary, or a turn map
// with gaps) fall back to the nearest turn; the caller lowers reported
// confidence for those.
func Assign(words []core.Word, turns []Turn) []string {
	out := make([]string, len(words))
	for i, w := range words {
		best, bestOverlap := -1, 0.0
		for _, t := range turns {
			ov := overlap(w.Start, w.End, t.Start, t.End)
			if ov > bestOverlap {
				best, bestOverlap = t.Speaker, ov
			}
		}
		if best < 0 {
			best = nearest(w, turns)
		}
		if best >= 0 {
			out[i] = speakerID(best)
		}
	}
	return out
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
