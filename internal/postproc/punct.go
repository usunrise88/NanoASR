package postproc

import (
	"context"
	"strings"
	"unicode"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Punctuator restores punctuation and capitalisation with a separate
// sherpa-onnx OfflinePunctuation model (CT-Transformer).
//
// It exists for models that do not punctuate themselves. Russian is not one of
// the languages it serves — no Russian CT-Transformer is published, and Russian
// punctuation comes from a punctuating ASR model instead (SPEC §5.6) — so this
// stage covers the zh/en case and the mechanism the spec asks for.
//
// Marks attach to the preceding word, so word boundaries never move. That is
// also what makes the stage one-to-one: it returns no span vector because it
// cannot change how many words there are.
type Punctuator struct {
	model AddPunct
}

// AddPunct is the model's side of the stage, kept as an interface so the
// re-attachment logic below is testable without cgo or weights.
type AddPunct interface {
	// AddPunct returns the text with marks and capitals restored.
	AddPunct(ctx context.Context, text string) (string, error)
}

func NewPunctuator(m AddPunct) *Punctuator { return &Punctuator{model: m} }

func (*Punctuator) Name() string { return "punctuation" }

// chunkWords is how many words go to the model at once, and overlap is how many
// are shared with the neighbouring chunk.
//
// A CT-Transformer has a bounded context, and one very long input degrades
// rather than fails. Chunking also bounds the blast radius of a re-attachment
// failure: one desynchronised chunk falls back to its own input instead of
// costing the whole file its punctuation.
const (
	chunkWords = 200
	overlap    = 20
)

func (p *Punctuator) Apply(ctx context.Context, in []core.Word) ([]core.Word, []int, error) {
	if len(in) == 0 {
		return in, nil, nil
	}
	out := make([]core.Word, 0, len(in))

	for start := 0; start < len(in); {
		end := min(start+chunkWords, len(in))
		// Take a little of what came before for context, but keep only the
		// words this chunk owns.
		ctxStart := max(start-overlap, 0)

		chunk := in[ctxStart:end]
		punctuated, err := p.model.AddPunct(ctx, joinPlain(chunk))
		if err != nil {
			return nil, nil, err
		}

		got, ok := reattach(chunk, punctuated)
		if !ok {
			// The model's output no longer lines up with the words it was
			// given. Keeping the words is strictly better than guessing where
			// the marks belong, and the caller turns this into a warning.
			got = chunk
		}
		out = append(out, got[start-ctxStart:]...)
		start = end
	}
	return out, nil, nil
}

func joinPlain(ws []core.Word) string {
	parts := make([]string, len(ws))
	for i, w := range ws {
		parts[i] = w.Word
	}
	return strings.Join(parts, " ")
}

// reattach maps a punctuated string back onto the words it came from.
//
// Both sides are walked letter by letter, ignoring whitespace and case, so a
// model that inserted marks and changed capitals still lines up. Anything the
// model added between two letters belongs to the word that ended last, which is
// what keeps a comma from becoming a word with a duration.
//
// It reports failure rather than doing its best: a mis-attached transcript
// looks correct and is not, whereas an unpunctuated one is obviously what it
// is.
func reattach(in []core.Word, punctuated string) ([]core.Word, bool) {
	out := make([]core.Word, len(in))
	copy(out, in)

	src := []rune(punctuated)
	pos := 0

	for i, word := range in {
		var b strings.Builder
		for _, want := range word.Word {
			if !unicode.IsLetter(want) && !unicode.IsDigit(want) {
				// Punctuation already on the input word: the model's own output
				// decides what the word carries now.
				continue
			}
			// Skip whatever the model inserted before this letter, attaching
			// any marks to the word being built.
			for pos < len(src) && !sameLetter(src[pos], want) {
				if unicode.IsSpace(src[pos]) {
					pos++
					continue
				}
				if unicode.IsLetter(src[pos]) || unicode.IsDigit(src[pos]) {
					// A letter that is not the one expected means the two
					// sides have diverged.
					return nil, false
				}
				b.WriteRune(src[pos])
				pos++
			}
			if pos >= len(src) {
				return nil, false
			}
			b.WriteRune(src[pos])
			pos++
		}
		if b.Len() == 0 {
			// A word with no letters at all, which reattach cannot anchor.
			return nil, false
		}
		out[i].Word = b.String()
		// Trailing marks belong to this word, not to the next one.
		for pos < len(src) && !unicode.IsSpace(src[pos]) &&
			!unicode.IsLetter(src[pos]) && !unicode.IsDigit(src[pos]) {
			out[i].Word += string(src[pos])
			pos++
		}
	}
	return out, true
}

// sameLetter compares ignoring case, which is the whole point: the model
// restores capitals and the input is lowercase.
func sameLetter(a, b rune) bool {
	return unicode.ToLower(a) == unicode.ToLower(b)
}

var _ Stage = (*Punctuator)(nil)
