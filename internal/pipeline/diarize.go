package pipeline

import (
	"context"
	"fmt"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/diarize"
)

// diarizeResult carries the second pass back into transcribe.
type diarizeResult struct {
	segments []core.Segment
	speakers []core.Speaker
}

// diarize runs the speaker pass and rewrites the segments with what it found.
//
// It is a second pass over the whole recording rather than over VAD segments,
// because a speaker change can happen inside one segment and the clustering
// needs the whole file to decide how many speakers there are at all.
func (p *Pipeline) diarize(
	ctx context.Context,
	req core.Request,
	tracks []audio.PCM,
	segs []core.Segment,
) (diarizeResult, []core.Warning, error) {
	if !req.Diarize {
		return diarizeResult{segments: segs}, nil, nil
	}

	// SPEC §5.7: split already separates the speakers by channel, and running
	// a clusterer over one leg of a call would invent divisions inside a single
	// voice. Skipping is the better answer, so this is not an _unavailable
	// code — strict mode must not fail a request that got the better answer.
	if p.channelMode(req) == core.ChannelSplit && len(tracks) > 1 {
		return diarizeResult{segments: segs}, []core.Warning{{
			Code: "diarization_skipped_split",
			Message: "channel_mode=split already separates the speakers by channel, " +
				"so diarization was not run",
		}}, nil
	}

	if p.diarizer == nil {
		return diarizeResult{segments: segs}, []core.Warning{{
			Code: "diarization_unavailable",
			Message: "diarization is not configured on this server; " +
				"set diarization.enabled with a segmentation and an embedding model",
		}}, nil
	}
	if len(segs) == 0 {
		return diarizeResult{segments: segs}, nil, nil
	}

	turns, err := p.diarizer.Process(ctx, tracks[0], req.NumSpeakers)
	if err != nil {
		return diarizeResult{}, nil, err
	}
	if len(turns) == 0 {
		return diarizeResult{segments: segs}, []core.Warning{{
			Code:    "diarization_found_no_speakers",
			Message: "the diarizer found no speaker turns; every word is unattributed",
		}}, nil
	}

	words := wordsOf(segs)
	attrs := diarize.Assign(words, turns)
	if !diarize.Apply(segs, attrs) {
		return diarizeResult{segments: segs}, []core.Warning{{
			Code:    "diarization_unavailable",
			Message: "speaker attribution did not line up with the transcript and was dropped",
		}}, nil
	}

	split := diarize.Split(segs)
	speakers := diarize.Speakers(split)

	// A caller who said how many people are on the recording has told us what
	// the answer should look like, so getting fewer is worth saying out loud.
	// Silence here is what makes "everything is spk_0" look like a bug in the
	// server rather than what it is: two voices the embedding model cannot
	// tell apart. Not an _unavailable code — the transcript is fine, and
	// strict mode should not reject it.
	var warn []core.Warning
	if req.NumSpeakers > 1 && len(speakers) < req.NumSpeakers {
		warn = append(warn, core.Warning{
			Code: "diarization_fewer_speakers",
			Message: fmt.Sprintf(
				"asked for %d speakers, separated %d from %d turns; "+
					"a different diarization.embedding_model or a lower "+
					"diarization.clustering.threshold separates similar voices better",
				req.NumSpeakers, len(speakers), len(turns)),
		})
	}
	return diarizeResult{segments: split, speakers: speakers}, warn, nil
}

// wordsOf flattens segment words in the order Apply expects to walk them.
func wordsOf(segs []core.Segment) []core.Word {
	n := 0
	for _, s := range segs {
		n += len(s.Words)
	}
	out := make([]core.Word, 0, n)
	for _, s := range segs {
		out = append(out, s.Words...)
	}
	return out
}
