// Package words turns model tokens into words with time spans.
//
// This is the piece the whole word-level-timestamps requirement rests on, so
// its contract is explicit and enforced by tests (SPEC §5.5):
//
//	0 <= w.Start < w.End <= segmentEnd
//	w[i].End <= w[i+1].Start
//	joining the words reproduces the recognised text
package words

import (
	"math"
	"strings"
	"unicode"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/core"
)

// wordBoundary is the SentencePiece marker that opens a new word.
//
// It is not the only form it arrives in: sherpa-onnx hands back tokens with the
// marker already rendered as a leading space, so English zipformer output looks
// like " AFTER", " E", "AR", "LY". Matching only the marker merged an entire
// sentence into one word — measured, not hypothetical.
const wordBoundary = "▁" // ▁

// ModelingUnit values, taken from the model manifest.
const (
	UnitBPE        = "bpe"
	UnitCJKChar    = "cjkchar"
	UnitCJKCharBPE = "cjkchar+bpe"
	UnitChar       = "char"
)

// Options describes the segment the tokens came from.
type Options struct {
	ModelingUnit string
	// SegmentStart and SegmentEnd are absolute seconds; token timestamps are
	// relative to SegmentStart.
	SegmentStart float64
	SegmentEnd   float64
	// WithConfidence emits per-word confidence derived from token log
	// probabilities. It is uncalibrated — good for ranking, not a probability.
	WithConfidence bool
}

// Assemble groups tokens into words and assigns each an absolute time span.
//
// When the recognition carries no timestamps, the caller is expected to fall
// back to segment-level timing rather than fabricate word times; Assemble
// returns nil in that case so the mistake cannot happen silently.
func Assemble(rec asr.Recognition, opt Options) []core.Word {
	if !rec.HasTimestamps() {
		return nil
	}

	type group struct{ first, last int }
	var groups []group
	breakPending := false

	for i, tok := range rec.Tokens {
		if isSpecial(tok) {
			continue
		}
		// A separator ends the current word without joining the next one.
		// Character-level vocabularies — GigaAM's Russian CTC, for instance —
		// emit an explicit space token with a frame of its own; folding it
		// into the following word would drag that word's start earlier by a
		// frame and leave a leading space in its text.
		if isSeparator(tok, opt.ModelingUnit) {
			breakPending = true
			continue
		}
		if len(groups) == 0 || breakPending || startsWord(tok, opt.ModelingUnit) {
			groups = append(groups, group{first: i, last: i})
			breakPending = false
			continue
		}
		groups[len(groups)-1].last = i
	}

	out := make([]core.Word, 0, len(groups))
	for _, g := range groups {
		text := cleanText(rec.Tokens[g.first : g.last+1])
		if text == "" {
			continue
		}

		start := opt.SegmentStart + float64(rec.Timestamps[g.first])
		end := opt.SegmentStart + tokenEnd(rec, g.last, opt.SegmentEnd-opt.SegmentStart)

		start = clamp(start, opt.SegmentStart, opt.SegmentEnd)
		end = clamp(end, start, opt.SegmentEnd)

		w := core.Word{Word: text, Start: start, End: end}
		if opt.WithConfidence {
			w.Confidence = confidence(rec.LogProbs, g.first, g.last)
		}
		out = append(out, w)
	}

	mergePunctuation(&out)
	enforceMonotonic(out)
	return out
}

// startsWord decides whether tok opens a new word under the given unit.
func startsWord(tok, unit string) bool {
	switch unit {
	case UnitCJKChar:
		return true
	case UnitCJKCharBPE:
		return opensWord(tok) || isCJK(tok)
	case UnitChar:
		// Characters accumulate; separators do the splitting.
		return false
	default: // UnitBPE and anything unrecognised: SentencePiece semantics
		return opensWord(tok)
	}
}

// opensWord reports a SentencePiece word-initial token in either form the
// binding produces: the marker itself, or the space it is rendered as.
func opensWord(tok string) bool {
	return strings.HasPrefix(tok, wordBoundary) || strings.HasPrefix(tok, " ")
}

// isSeparator reports a token that delimits words but is not part of one.
//
// SentencePiece encodes the boundary as a prefix on the following token, so
// only character vocabularies have a standalone separator. Both an ASCII space
// and a bare SentencePiece marker count as one.
func isSeparator(tok, unit string) bool {
	if unit != UnitChar {
		return false
	}
	return strings.TrimSpace(strings.ReplaceAll(tok, wordBoundary, "")) == ""
}

// tokenEnd resolves the end of the token at index i, in segment-relative
// seconds. Durations are the best source; the next token's onset is the usual
// fallback; the segment end is the last resort.
func tokenEnd(rec asr.Recognition, i int, segmentLen float64) float64 {
	if i < len(rec.Durations) && rec.Durations[i] > 0 {
		return float64(rec.Timestamps[i] + rec.Durations[i])
	}
	if i+1 < len(rec.Timestamps) {
		return float64(rec.Timestamps[i+1])
	}
	return segmentLen
}

func cleanText(tokens []string) string {
	var b strings.Builder
	for _, t := range tokens {
		b.WriteString(strings.ReplaceAll(t, wordBoundary, ""))
	}
	return strings.TrimSpace(b.String())
}

// isSpecial filters decoder bookkeeping tokens such as <blk>, <unk>, <sos/eos>
// and SenseVoice's <|…|> tags, which carry no text and no meaningful timing.
func isSpecial(tok string) bool {
	t := strings.TrimPrefix(tok, wordBoundary)
	return strings.HasPrefix(t, "<") && strings.HasSuffix(t, ">")
}

func isCJK(tok string) bool {
	for _, r := range strings.TrimPrefix(tok, wordBoundary) {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			return true
		}
		return false
	}
	return false
}

// confidence is exp(mean(log p)) over the token span. Uncalibrated by
// construction: it ranks uncertain words, it does not estimate correctness.
func confidence(logProbs []float32, first, last int) float64 {
	if first >= len(logProbs) {
		return 0
	}
	if last >= len(logProbs) {
		last = len(logProbs) - 1
	}
	sum, n := 0.0, 0
	for i := first; i <= last; i++ {
		sum += float64(logProbs[i])
		n++
	}
	if n == 0 {
		return 0
	}
	return math.Exp(sum / float64(n))
}

// mergePunctuation attaches a standalone punctuation word to its predecessor,
// extending that word's span. A comma is not a word with a duration.
func mergePunctuation(ws *[]core.Word) {
	in := *ws
	out := in[:0]
	for _, w := range in {
		if len(out) > 0 && isPunctuation(w.Word) {
			prev := &out[len(out)-1]
			prev.Word += w.Word
			if w.End > prev.End {
				prev.End = w.End
			}
			continue
		}
		out = append(out, w)
	}
	*ws = out
}

func isPunctuation(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsPunct(r) && !unicode.IsSymbol(r) {
			return false
		}
	}
	return true
}

// enforceMonotonic removes overlaps that rounding or duration estimates can
// introduce, so a player never sees two words active at once.
func enforceMonotonic(ws []core.Word) {
	for i := 0; i+1 < len(ws); i++ {
		if ws[i].End > ws[i+1].Start {
			ws[i].End = ws[i+1].Start
		}
		if ws[i].End < ws[i].Start {
			ws[i].End = ws[i].Start
		}
	}
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Text joins words back into a single string.
func Text(ws []core.Word) string {
	parts := make([]string, 0, len(ws))
	for _, w := range ws {
		parts = append(parts, w.Word)
	}
	return strings.Join(parts, " ")
}

// FromSegment builds a single segment-level pseudo-word span, used when the
// model cannot produce token timestamps. The caller must set the result's
// TimestampSource to segment so clients can tell the difference.
func FromSegment(text string, start, end float64) []core.Word {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return []core.Word{{Word: strings.TrimSpace(text), Start: start, End: end}}
}
