// Package postproc holds the optional text stages that run after recognition.
//
// The single rule every stage obeys: it may rewrite text, but it may never
// break the word↔time link (SPEC §5.6). A stage takes words and returns words,
// and each output word must map to a contiguous run of input words so its span
// is [start(first), end(last)].
package postproc

import (
	"context"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Stage transforms words while preserving timing.
//
// spans says, for each output word, how many consecutive input words it
// consumed. A nil spans means one-to-one, which is what a stage that only
// rewrites text returns. Anything that merges words has to say so, because the
// caller has to put the result back into the segments it came from and cannot
// work out which output belongs where by looking at the text.
type Stage interface {
	Name() string
	Apply(ctx context.Context, in []core.Word) (out []core.Word, spans []int, err error)
}

// Chain runs stages in order, stopping at the first error.
type Chain []Stage

func (c Chain) Apply(ctx context.Context, in []core.Word) ([]core.Word, []int, error) {
	out := in
	var spans []int
	for _, s := range c {
		got, gotSpans, err := s.Apply(ctx, out)
		if err != nil {
			return nil, nil, err
		}
		if err := checkSpans(s.Name(), len(out), got, gotSpans); err != nil {
			return nil, nil, err
		}
		out = got
		spans = composeSpans(spans, gotSpans, len(out))
	}
	return out, spans, nil
}

// checkSpans refuses a stage whose bookkeeping does not add up.
//
// Every future stage has to maintain this vector, and a wrong one does not
// produce a visible error — it silently misattributes text to the wrong slice
// of the recording. Failing loudly here is the difference between a bug that is
// found in a test and one that is found in a transcript.
func checkSpans(stage string, inputs int, out []core.Word, spans []int) error {
	if spans == nil {
		if len(out) != inputs {
			return core.Errorf(core.CodeInternal,
				"postproc stage %s returned %d words for %d inputs without a span vector",
				stage, len(out), inputs)
		}
		return nil
	}
	if len(spans) != len(out) {
		return core.Errorf(core.CodeInternal,
			"postproc stage %s returned %d words and %d spans", stage, len(out), len(spans))
	}
	total := 0
	for _, n := range spans {
		if n < 1 {
			return core.Errorf(core.CodeInternal,
				"postproc stage %s produced a word covering %d inputs", stage, n)
		}
		total += n
	}
	if total != inputs {
		return core.Errorf(core.CodeInternal,
			"postproc stage %s spans cover %d of %d input words", stage, total, inputs)
	}
	return nil
}

// composeSpans folds a stage's spans into the running total, so the vector
// always describes the distance back to the words the pipeline started with
// rather than to whatever the previous stage produced.
func composeSpans(outer, inner []int, outputs int) []int {
	switch {
	case inner == nil:
		return outer
	case outer == nil:
		return inner
	}
	out := make([]int, 0, outputs)
	cursor := 0
	for _, n := range inner {
		total := 0
		for range n {
			total += outer[cursor]
			cursor++
		}
		out = append(out, total)
	}
	return out
}
