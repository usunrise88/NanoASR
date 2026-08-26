package postproc

import (
	"context"
	"sort"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/words"
)

// Apply runs a chain over the whole transcript and puts the result back into
// the segments it came from.
//
// The chain sees one flat stream rather than one segment at a time, and that is
// the point. Punctuation quality depends on sentence context, and a sentence
// routinely spans two VAD segments; an ITN span like "двадцать пять рублей" can
// straddle the same boundary. Running per segment would cut both off at a line
// the speaker never drew.
func Apply(ctx context.Context, c Chain, segs []core.Segment) ([]core.Segment, error) {
	if len(c) == 0 || len(segs) == 0 {
		return segs, nil
	}

	flat, layout := flatten(segs)
	if len(flat) == 0 {
		return segs, nil
	}

	out, spans, err := c.Apply(ctx, flat)
	if err != nil {
		return nil, err
	}
	return scatter(segs, out, spans, layout), nil
}

// flatten returns every word in transcript order, plus how many belong to each
// segment.
func flatten(segs []core.Segment) ([]core.Word, []int) {
	layout := make([]int, len(segs))
	n := 0
	for i, s := range segs {
		layout[i] = len(s.Words)
		n += len(s.Words)
	}
	flat := make([]core.Word, 0, n)
	for _, s := range segs {
		flat = append(flat, s.Words...)
	}
	return flat, layout
}

// scatter puts rewritten words back into segments.
//
// A word that merged inputs from two segments goes to the segment that owned
// its first input word. Ownership has to be decided somehow and this is the
// rule that keeps a merge next to the words it came from; the ITN rules refuse
// to merge across a long pause, so in practice a merge only crosses a boundary
// when that boundary was nearly silent.
func scatter(segs []core.Segment, out []core.Word, spans, layout []int) []core.Segment {
	buckets := make([][]core.Word, len(segs))

	seg, seenInSeg, cursor := 0, 0, 0
	for i, w := range out {
		n := 1
		if spans != nil {
			n = spans[i]
		}
		// Advance to the segment that owns this word's first input.
		for seg < len(segs)-1 && seenInSeg >= layout[seg] {
			seg++
			seenInSeg = 0
		}
		buckets[seg] = append(buckets[seg], w)

		// Consume the inputs this word covered, walking segments as it goes.
		for range n {
			seenInSeg++
			cursor++
			for seg < len(segs)-1 && seenInSeg >= layout[seg] {
				seg++
				seenInSeg = 0
			}
		}
	}

	result := make([]core.Segment, 0, len(segs))
	for i, s := range segs {
		ws := buckets[i]
		if len(ws) == 0 {
			// Every word of this segment merged into a neighbour. An empty
			// segment is not a segment.
			continue
		}
		s.Words = ws
		s.Text = words.Text(ws)
		s.AvgConfidence = averageConfidence(ws)
		retime(&s)
		result = append(result, s)
	}
	enforceOrder(result)
	for i := range result {
		result[i].ID = i
	}
	return result
}

// retime widens a segment to cover the words it now holds. Its outer edges came
// from VAD and are kept unless a word fell outside them, which a merge across a
// boundary can do.
func retime(s *core.Segment) {
	if len(s.Words) == 0 {
		return
	}
	if first := s.Words[0].Start; first < s.Start {
		s.Start = first
	}
	if last := s.Words[len(s.Words)-1].End; last > s.End {
		s.End = last
	}
}

// enforceOrder stops a widened segment from overlapping the next one, which a
// player would render as two segments active at once.
func enforceOrder(segs []core.Segment) {
	for i := 0; i+1 < len(segs); i++ {
		if segs[i+1].Start < segs[i].End {
			segs[i+1].Start = segs[i].End
		}
		if segs[i+1].End < segs[i+1].Start {
			segs[i+1].End = segs[i+1].Start
		}
	}
}

func averageConfidence(ws []core.Word) float64 {
	sum, n := 0.0, 0
	for _, w := range ws {
		if w.Confidence > 0 {
			sum += w.Confidence
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// ApplyWithChannels runs the chain against one channel at a time when the
// segments carry more than one channel, and otherwise falls through to the
// ordinary Apply. Each leg of a stereo recording is its own conversation: an
// ITN rule like "join two words within 350 ms into a number" must not glue a
// word from channel 0 onto one from channel 1. After per-leg processing the
// streams merge back in (Start, Channel) order, which is the order assemble
// already produced.
func ApplyWithChannels(ctx context.Context, c Chain, segs []core.Segment) ([]core.Segment, error) {
	if len(segs) == 0 || len(c) == 0 {
		return segs, nil
	}
	if !hasMultipleChannels(segs) {
		return Apply(ctx, c, segs)
	}
	return applyPerChannel(ctx, c, segs)
}

func hasMultipleChannels(segs []core.Segment) bool {
	if len(segs) == 0 {
		return false
	}
	seen := map[int]struct{}{}
	for _, s := range segs {
		if len(s.Words) == 0 {
			continue
		}
		ch := s.Words[0].Channel
		if _, ok := seen[ch]; ok {
			continue
		}
		seen[ch] = struct{}{}
		if len(seen) > 1 {
			return true
		}
	}
	return false
}

func applyPerChannel(ctx context.Context, c Chain, segs []core.Segment) ([]core.Segment, error) {
	byChannel := map[int][]core.Segment{}
	order := []int{}
	for _, s := range segs {
		ch := 0
		if len(s.Words) > 0 {
			ch = s.Words[0].Channel
		}
		if _, ok := byChannel[ch]; !ok {
			order = append(order, ch)
		}
		byChannel[ch] = append(byChannel[ch], s)
	}

	rewritten := make([][]core.Segment, len(order))
	for i, ch := range order {
		out, err := Apply(ctx, c, byChannel[ch])
		if err != nil {
			return nil, err
		}
		rewritten[i] = out
	}

	merged := make([]core.Segment, 0, len(segs))
	for _, group := range rewritten {
		merged = append(merged, group...)
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].Start != merged[j].Start {
			return merged[i].Start < merged[j].Start
		}
		ci, cj := 0, 0
		if len(merged[i].Words) > 0 {
			ci = merged[i].Words[0].Channel
		}
		if len(merged[j].Words) > 0 {
			cj = merged[j].Words[0].Channel
		}
		return ci < cj
	})
	for i := range merged {
		merged[i].ID = i
	}
	return merged, nil
}
