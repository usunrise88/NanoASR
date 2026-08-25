package postproc

import (
	"context"
	"strings"
	"testing"

	"github.com/usunrise88/nanoasr/internal/core"
)

// mergePairs is a stage that joins every two words, which is the simplest thing
// that exercises the span vector without depending on any locale's rules.
type mergePairs struct{}

func (mergePairs) Name() string { return "merge-pairs" }

func (mergePairs) Apply(_ context.Context, in []core.Word) ([]core.Word, []int, error) {
	var out []core.Word
	var spans []int
	for i := 0; i < len(in); i += 2 {
		if i+1 >= len(in) {
			out = append(out, in[i])
			spans = append(spans, 1)
			break
		}
		out = append(out, core.Word{
			Word:     in[i].Word + in[i+1].Word,
			Start:    in[i].Start,
			End:      in[i+1].End,
			Original: in[i].Word + " " + in[i+1].Word,
		})
		spans = append(spans, 2)
	}
	return out, spans, nil
}

// upper rewrites text one-to-one and returns no span vector, which is what a
// punctuation stage does.
type upper struct{}

func (upper) Name() string { return "upper" }
func (upper) Apply(_ context.Context, in []core.Word) ([]core.Word, []int, error) {
	out := make([]core.Word, len(in))
	copy(out, in)
	for i := range out {
		out[i].Word = strings.ToUpper(out[i].Word)
	}
	return out, nil, nil
}

func seg(start, end float64, words ...core.Word) core.Segment {
	return core.Segment{Start: start, End: end, Text: joinWords(words), Words: words}
}

func joinWords(ws []core.Word) string {
	parts := make([]string, len(ws))
	for i, w := range ws {
		parts[i] = w.Word
	}
	return strings.Join(parts, " ")
}

func w(text string, start, end float64) core.Word {
	return core.Word{Word: text, Start: start, End: end}
}

// The ordinary case: a stage that rewrites without merging leaves the segment
// structure exactly as it was.
func TestApplyKeepsSegmentsWhenNothingMerges(t *testing.T) {
	segs := []core.Segment{
		seg(0, 1, w("да", 0, 0.4), w("конечно", 0.5, 1)),
		seg(2, 3, w("хорошо", 2, 2.5)),
	}
	got, err := Apply(context.Background(), Chain{upper{}}, segs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d segments, want 2", len(got))
	}
	if got[0].Text != "ДА КОНЕЧНО" || got[1].Text != "ХОРОШО" {
		t.Errorf("texts = %q / %q", got[0].Text, got[1].Text)
	}
	// Segment boundaries came from VAD and nothing here should move them.
	if got[0].Start != 0 || got[0].End != 1 {
		t.Errorf("first segment moved to %v-%v", got[0].Start, got[0].End)
	}
}

// The rule the plan singled out for a direct test: a merge that crosses a
// segment boundary belongs to the segment that owned its first input word.
// Production will almost never do this, because the ITN rules refuse to merge
// across a long pause — which is exactly why it needs testing here.
func TestApplyOwnershipOfACrossBoundaryMerge(t *testing.T) {
	segs := []core.Segment{
		seg(0, 1.0, w("двадцать", 0.6, 1.0)),
		seg(1.1, 2.0, w("пять", 1.1, 1.5)),
	}
	got, err := Apply(context.Background(), Chain{mergePairs{}}, segs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d segments, want 1: the second gave up its only word", len(got))
	}
	if got[0].Text != "двадцатьпять" {
		t.Errorf("text = %q", got[0].Text)
	}
	// The merged word ends at 1.5, past the first segment's VAD end of 1.0, so
	// the segment has to widen to still contain its own words.
	if got[0].End != 1.5 {
		t.Errorf("segment ends at %v, want 1.5 to cover the word it now holds", got[0].End)
	}
	if got[0].ID != 0 {
		t.Errorf("ID = %d, want ids renumbered after a segment disappeared", got[0].ID)
	}
}

// A widened segment must not overlap the one after it: a player would show two
// segments active at once.
func TestApplyKeepsSegmentsInOrder(t *testing.T) {
	segs := []core.Segment{
		seg(0, 1.0, w("двадцать", 0.6, 1.0), w("пять", 1.1, 1.6)),
		seg(1.2, 3.0, w("рублей", 1.2, 1.8), w("итого", 2.0, 2.6)),
	}
	got, err := Apply(context.Background(), Chain{mergePairs{}}, segs)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(got); i++ {
		if got[i+1].Start < got[i].End {
			t.Errorf("segment %d ends at %v but %d starts at %v",
				i, got[i].End, i+1, got[i+1].Start)
		}
	}
}

// Chain composes spans so the vector always describes the distance back to the
// words the pipeline started with, not to the previous stage's output.
func TestChainComposesSpansAcrossStages(t *testing.T) {
	in := []core.Word{w("а", 0, 0.1), w("б", 0.2, 0.3), w("в", 0.4, 0.5), w("г", 0.6, 0.7)}

	out, spans, err := Chain{mergePairs{}, mergePairs{}}.Apply(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d words, want 1 after two halvings", len(out))
	}
	if len(spans) != 1 || spans[0] != 4 {
		t.Errorf("spans = %v, want [4] covering every original word", spans)
	}
}

// A stage whose bookkeeping does not add up is an internal error, not a
// silently misattributed transcript.
func TestChainRejectsBadSpans(t *testing.T) {
	_, _, err := Chain{badSpans{}}.Apply(context.Background(),
		[]core.Word{w("а", 0, 1), w("б", 1, 2)})
	if err == nil {
		t.Fatal("a stage claiming the wrong number of inputs must be refused")
	}
	if !strings.Contains(err.Error(), "bad-spans") {
		t.Errorf("error %q should name the offending stage", err)
	}
}

type badSpans struct{}

func (badSpans) Name() string { return "bad-spans" }
func (badSpans) Apply(_ context.Context, in []core.Word) ([]core.Word, []int, error) {
	// Claims to have merged three words when it was given two.
	return in[:1], []int{3}, nil
}

func TestApplyWithAnEmptyChainIsANoOp(t *testing.T) {
	segs := []core.Segment{seg(0, 1, w("да", 0, 0.4))}
	got, err := Apply(context.Background(), nil, segs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "да" {
		t.Errorf("segments = %+v", got)
	}
}
