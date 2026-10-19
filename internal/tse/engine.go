package tse

import (
	"context"
	"errors"
	"math"

	"github.com/usunrise88/nanoasr/internal/tse/voxlock"
)

// Engine is voice isolation end to end: a voice sample per speaker, then one
// speaker's voice out of the recording. It holds no per-recording state and is
// safe for concurrent use.
type Engine struct {
	Model     *voxlock.Model
	Embedder  Embedder
	Selection Selection
	Gate      Gate
}

// Voice is what Samples found for one speaker: a sample, or why there is none.
type Voice struct {
	Speaker Speaker
	Sample  Sample
	// Err is set when the speaker cannot be isolated, and Sample is empty.
	Err error
}

// Samples chooses a voice sample for every speaker. A speaker with too
// little clean speech gets Err rather than failing the rest.
func (e *Engine) Samples(ctx context.Context, audio []float32, speakers []Speaker) ([]Voice, error) {
	out := make([]Voice, 0, len(speakers))
	for _, sp := range speakers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s, err := ChooseSample(audio, sp, speakers, e.Embedder, e.Selection)
		var short ErrTooLittleSpeech
		switch {
		case errors.As(err, &short):
			out = append(out, Voice{Speaker: sp, Err: err})
		case err != nil:
			return nil, err
		default:
			out = append(out, Voice{Speaker: sp, Sample: s})
		}
	}
	return out, nil
}

// extractShare is the part of Extract's progress the extractor takes; the
// gate is the rest. Measured: the extractor is about nine tenths of the time.
const extractShare = 0.9

// Regions of the recording are extracted each from a fresh state, so the
// extractor starts every turn of the chosen voice the way it starts a
// recording. Carried across a stretch of somebody else, the state lets the
// voice go: measured on a Russian dialogue, after six seconds of the other
// speaker the extractor took two seconds to find the chosen voice again and
// muted the start of its turn, where a fresh state kept it whole.
const (
	// regionMerge joins turns of the chosen voice closer than this: a
	// breath is not a hand-over.
	regionMerge = 0.5
	// regionPreRoll is audio fed before a region and dropped from the
	// output, so a region does not start on an empty state mid-word.
	regionPreRoll = 0.5
)

// Extract returns audio with only the voice of sample left in it, exactly as
// long as the input. others are the embeddings of the other voices' samples,
// which the gate tells the chosen one from; keep are the speaker's own turns,
// which it leaves to the extractor and which mark where a fresh state starts.
// nil keep extracts the whole recording in one pass and gates everything by
// the embeddings alone. progress, when set, is told the fraction done.
func (e *Engine) Extract(ctx context.Context, audio []float32, sample Sample, others [][]float32, keep []Span, progress func(float64)) ([]float32, error) {
	report := func(f float64) {
		if progress != nil {
			progress(f)
		}
	}
	v, err := e.Model.Enroll(sample.Audio)
	if err != nil {
		return nil, err
	}
	regs := regions(len(audio), keep)
	total := 0
	for _, r := range regs {
		total += r.end - r.feed
	}
	var y []float32
	done := 0
	for _, r := range regs {
		part, err := e.Model.Isolate(ctx, audio[r.feed:r.end], v, func(n int) {
			report(extractShare * float64(done+n) / float64(max(1, total)))
		})
		if err != nil {
			return nil, err
		}
		// One region is the whole recording, and its output is the result;
		// a second copy of an hour of audio is not free.
		if len(regs) == 1 {
			y = part
			break
		}
		if y == nil {
			y = make([]float32, len(audio))
		}
		copy(y[r.start:r.end], part[r.start-r.feed:])
		done += r.end - r.feed
	}
	g := e.Gate
	g.Others, g.Keep = others, keep
	n := float64(max(1, len(audio)))
	out, err := g.Apply(ctx, y, sample.Embedding, e.Embedder, func(done int) {
		report(extractShare + (1-extractShare)*float64(done)/n)
	})
	if err != nil {
		return nil, err
	}
	report(1)
	return out, nil
}

// region is a stretch extracted from a fresh state: fed from feed, kept from
// start, both in samples.
type region struct{ feed, start, end int }

// regions cuts a recording of n samples at the edges of the chosen voice's
// turns, so each turn and each stretch between them is its own region.
func regions(n int, keep []Span) []region {
	at := func(t float64) int { return min(n, max(0, int(math.Round(t*SampleRate)))) }
	var cuts []int
	var last Span
	for i, s := range merge(keep) {
		if i > 0 && s.Start-last.End < regionMerge {
			cuts = cuts[:len(cuts)-1]
			cuts = append(cuts, at(s.End))
			last.End = s.End
			continue
		}
		cuts = append(cuts, at(s.Start), at(s.End))
		last = s
	}
	bounds := []int{0}
	for _, c := range cuts {
		if c > bounds[len(bounds)-1] && c < n {
			bounds = append(bounds, c)
		}
	}
	bounds = append(bounds, n)
	pre := int(regionPreRoll * SampleRate)
	var out []region
	for i := 0; i+1 < len(bounds); i++ {
		if bounds[i] == bounds[i+1] {
			continue
		}
		out = append(out, region{feed: max(0, bounds[i]-pre), start: bounds[i], end: bounds[i+1]})
	}
	return out
}

// Embed is the engine's speaker embedding, for a sample placed by hand.
func (e *Engine) Embed(samples []float32) ([]float32, error) { return e.Embedder.Embed(samples) }
