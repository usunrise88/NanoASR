package itn

import (
	"strings"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Rewrite applies a locale's rules to a word stream.
//
// It returns the rewritten words and, for each one, how many inputs it
// consumed — the span vector postproc.Stage is defined in terms of. A rewritten
// word spans [start(first), end(last)] of the run it replaced and keeps what
// was said in Original, so a client can show either form and the player can
// still seek to it.
func Rewrite(rules Rules, in []core.Word) ([]core.Word, []int) {
	if rules == nil || len(in) == 0 {
		return in, nil
	}

	tokens := make([]Token, len(in))
	for i, w := range in {
		// Lower keeps any trailing punctuation: a rule needs to see the mark to
		// carry it through, and trimPunct is how it looks past one.
		tokens[i] = Token{
			Text:  w.Word,
			Lower: strings.ToLower(w.Word),
			Start: w.Start,
			End:   w.End,
		}
	}

	out := make([]core.Word, 0, len(in))
	spans := make([]int, 0, len(in))
	rewrote := false

	for i := 0; i < len(in); {
		m, ok := rules.Match(tokens[i:])
		if !ok || m.Count < 1 || i+m.Count > len(in) {
			out = append(out, in[i])
			spans = append(spans, 1)
			i++
			continue
		}

		first, last := in[i], in[i+m.Count-1]
		w := core.Word{
			Word:  m.Text,
			Start: first.Start,
			End:   last.End,
			// The span's confidence is its weakest link: a rewrite is only as
			// trustworthy as the least certain word it swallowed.
			Confidence:        minConfidence(in[i : i+m.Count]),
			Original:          originalOf(in[i : i+m.Count]),
			Speaker:           first.Speaker,
			SpeakerConfidence: first.SpeakerConfidence,
			Channel:           first.Channel,
		}
		if w.Word == w.Original {
			// A rule that changed nothing is not a rewrite, and recording an
			// Original identical to the word would only confuse a client.
			w.Original = ""
		} else {
			rewrote = true
		}
		out = append(out, w)
		spans = append(spans, m.Count)
		i += m.Count
	}

	if !rewrote {
		// Nothing changed, so hand back the input and no span vector: the
		// caller then skips the scatter entirely.
		return in, nil
	}
	return out, spans
}

// originalOf reconstructs what was actually said across a rewritten run.
func originalOf(ws []core.Word) string {
	parts := make([]string, 0, len(ws))
	for _, w := range ws {
		if w.Original != "" {
			parts = append(parts, w.Original)
			continue
		}
		parts = append(parts, w.Word)
	}
	return strings.Join(parts, " ")
}

func minConfidence(ws []core.Word) float64 {
	out := 0.0
	for i, w := range ws {
		if i == 0 || w.Confidence < out {
			out = w.Confidence
		}
	}
	return out
}
