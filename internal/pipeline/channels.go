package pipeline

import (
	"context"
	"sort"
	"strings"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/pool"
	"github.com/usunrise88/nanoasr/internal/vad"
)

// track is what one decoded channel contributed.
type track struct {
	segments     []core.Segment
	spans        []vad.Span
	samples      int
	vadSegments  int
	tokenTimings bool
}

// channel runs vad, asr and word assembly for one decoded channel.
//
// Channels run one after another rather than concurrently. The governor
// already saturates the CPU with the threads of a single decode, so a second
// channel in flight buys no throughput — it only doubles the peak memory, on
// exactly the long files where that is the constraint.
func (p *Pipeline) channel(
	ctx context.Context,
	stages *stageTimer,
	lease *pool.Lease,
	req core.Request,
	pcm audio.PCM,
) (track, error) {
	segments, err := runStage(stages, "vad", func() ([]vad.Segment, error) {
		return p.segmenter.Segment(ctx, pcm)
	})
	if err != nil {
		return track{}, err
	}

	recognitions, err := runStage(stages, "asr", func() ([]asr.Recognition, error) {
		return p.recognise(ctx, lease.Recognizer, segments, pcm.SampleRate)
	})
	if err != nil {
		return track{}, err
	}

	built, _ := runStage(stages, "assemble", func() (track, error) {
		segs, saw := p.buildSegments(lease, pcm, segments, recognitions)
		return track{
			segments:     segs,
			spans:        vad.Spans(segments),
			samples:      len(pcm.Samples),
			vadSegments:  len(segments),
			tokenTimings: saw,
		}, nil
	})
	return built, nil
}

// merge orders the segments of every channel into one transcript.
//
// Sorting by start time is what makes a two-legged call read as a conversation
// rather than as one side followed by the other. The channel breaks ties so the
// order is stable when both legs start on the same sample, and ids are handed
// out afterwards because a client uses them to index this list, not to learn
// which channel a segment came from — Segment.Channel says that.
func merge(tracks []track) []core.Segment {
	var out []core.Segment
	for _, t := range tracks {
		out = append(out, t.segments...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		return out[i].Channel < out[j].Channel
	})
	for i := range out {
		out[i].ID = i
	}
	return out
}

// joinSegments is the transcript as one string.
//
// It lives here rather than inline in assemble because post-processing rewrites
// segment text and has to rebuild the same string the same way; two copies of
// this would be two places for the whole-text and the per-segment text to drift
// apart.
func joinSegments(segs []core.Segment) string {
	parts := make([]string, 0, len(segs))
	for _, s := range segs {
		if s.Text != "" {
			parts = append(parts, s.Text)
		}
	}
	return strings.Join(parts, " ")
}
