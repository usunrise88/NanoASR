package pipeline

import (
	"context"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/postproc"
)

// post runs the optional text stages and rewrites the result with what they
// produced.
//
// It runs after diarization rather than before it. Diarization attributes words
// by time, and ITN merges words — running the merge first would hand the
// diarizer a word spanning two speakers with no way to split it back.
func (p *Pipeline) post(
	ctx context.Context,
	req core.Request,
	caps core.Capabilities,
	result *core.Result,
) ([]core.Warning, error) {
	// A server wired without a post-processing block still answers the
	// question honestly: a zero factory has no punctuation model and no
	// configured defaults, so it reports what it cannot do rather than staying
	// silent. Inverse text normalisation needs no model at all, so it works
	// here as it does anywhere else.
	factory := p.postproc
	if factory == nil {
		factory = &postproc.Factory{}
	}

	chain, warn := factory.Chain(req, caps, result.Language)
	if len(chain) == 0 {
		return warn, nil
	}

	segs, err := postproc.Apply(ctx, chain, result.Segments)
	if err != nil {
		// A stage that failed is a degradation, not a lost transcript: the
		// words are already correct, only their spelling is not what was asked
		// for. Deliberately not an _unavailable code, so strict mode does not
		// reject a request that has a usable answer.
		return append(warn, core.Warning{
			Code:    "postprocessing_degraded",
			Message: core.AsError(err).Message + "; the transcript is unmodified",
		}), nil
	}

	result.Segments = segs
	result.Text = joinSegments(segs)
	return warn, nil
}
