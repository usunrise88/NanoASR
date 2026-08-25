package itn

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/usunrise88/nanoasr/internal/core"
)

// wordsOnAGrid lays a spoken phrase out at 0.4s per word, which is roughly what
// a recogniser reports and comfortably inside MaxGap, so adjacency is never the
// thing under test.
func wordsOnAGrid(phrase string) []core.Word {
	fields := strings.Fields(phrase)
	out := make([]core.Word, len(fields))
	for i, f := range fields {
		out[i] = core.Word{
			Word:  f,
			Start: float64(i) * 0.4,
			End:   float64(i)*0.4 + 0.35,
		}
	}
	return out
}

func rendered(ws []core.Word) string {
	parts := make([]string, len(ws))
	for i, w := range ws {
		parts[i] = w.Word
	}
	return strings.Join(parts, " ")
}

// TestRussianGoldenCorpus is the contract for the rule set. A rule that changes
// behaviour has to change this file, which makes the change visible in review
// rather than only in a transcript.
func TestRussianGoldenCorpus(t *testing.T) {
	f, err := os.Open("testdata/ru.golden")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	rules, ok := Get("ru")
	if !ok {
		t.Fatal("the ru rules are not registered")
	}

	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		spoken, want, found := strings.Cut(text, "\t")
		if !found {
			t.Fatalf("%s:%d: no tab separator", "ru.golden", line)
		}
		spoken, want = strings.TrimSpace(spoken), strings.TrimSpace(want)

		t.Run(spoken, func(t *testing.T) {
			in := wordsOnAGrid(spoken)
			out, spans := Rewrite(rules, in)
			if got := rendered(out); got != want {
				t.Errorf("Rewrite(%q) = %q, want %q", spoken, got, want)
			}
			assertSpansAndTiming(t, in, out, spans)
		})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
}

// assertSpansAndTiming enforces SPEC §5.6's rule that no stage may break the
// word↔time link. Every output word must cover a contiguous run of inputs and
// carry exactly that run's span.
func assertSpansAndTiming(t *testing.T, in, out []core.Word, spans []int) {
	t.Helper()

	if spans == nil {
		if len(out) != len(in) {
			t.Fatalf("no span vector but %d words became %d", len(in), len(out))
		}
		return
	}
	if len(spans) != len(out) {
		t.Fatalf("%d words and %d spans", len(out), len(spans))
	}

	cursor := 0
	for i, n := range spans {
		if n < 1 {
			t.Fatalf("word %d covers %d inputs", i, n)
		}
		if cursor+n > len(in) {
			t.Fatalf("word %d runs past the input", i)
		}
		first, last := in[cursor], in[cursor+n-1]
		if out[i].Start != first.Start {
			t.Errorf("word %q starts at %v, its first input at %v", out[i].Word, out[i].Start, first.Start)
		}
		if out[i].End != last.End {
			t.Errorf("word %q ends at %v, its last input at %v", out[i].Word, out[i].End, last.End)
		}
		if n > 1 {
			original := rendered(in[cursor : cursor+n])
			if out[i].Original != original {
				t.Errorf("word %q kept Original %q, want %q", out[i].Word, out[i].Original, original)
			}
		}
		cursor += n
	}
	if cursor != len(in) {
		t.Errorf("spans cover %d of %d input words", cursor, len(in))
	}
}

// A pause in the middle is a sentence boundary. "двадцать" ending one thought
// and "пять" beginning the next must not become 25.
func TestRewriteWillNotMergeAcrossAPause(t *testing.T) {
	rules, _ := Get("ru")
	in := []core.Word{
		{Word: "двадцать", Start: 0, End: 0.5},
		{Word: "пять", Start: 0.5 + MaxGap + 0.1, End: 1.5},
	}
	out, _ := Rewrite(rules, in)
	// Each word is still normalised on its own — "двадцать" is a number
	// whatever follows it — but they must not become 25.
	if got := rendered(out); got != "20 пять" {
		t.Errorf("Rewrite = %q, want the two words normalised separately", got)
	}
	if len(out) != 2 {
		t.Errorf("got %d words, want the pause to have kept them apart", len(out))
	}
}

// The same words with no pause do merge, so the test above is measuring the gap
// rather than a rule that never fires.
func TestRewriteMergesWithoutAPause(t *testing.T) {
	rules, _ := Get("ru")
	in := []core.Word{
		{Word: "двадцать", Start: 0, End: 0.5},
		{Word: "пять", Start: 0.6, End: 1.0},
	}
	out, _ := Rewrite(rules, in)
	if got := rendered(out); got != "25" {
		t.Errorf("Rewrite = %q, want 25", got)
	}
	if out[0].Original != "двадцать пять" {
		t.Errorf("Original = %q, want what was said", out[0].Original)
	}
}

// A punctuating model attaches marks to words. The mark has to survive the
// rewrite, or ITN would silently delete the punctuation the model produced.
func TestRewriteCarriesPunctuationThrough(t *testing.T) {
	rules, _ := Get("ru")
	in := wordsOnAGrid("двадцать пять,")
	out, _ := Rewrite(rules, in)
	if got := rendered(out); got != "25," {
		t.Errorf("Rewrite = %q, want the comma kept", got)
	}
}

// Speaker attribution is set before post-processing runs, so a merge has to
// carry it rather than dropping it.
func TestRewriteKeepsSpeakerAttribution(t *testing.T) {
	rules, _ := Get("ru")
	spk := "spk_1"
	in := []core.Word{
		{Word: "двадцать", Start: 0, End: 0.4, Speaker: &spk, SpeakerConfidence: 0.9, Channel: 1},
		{Word: "пять", Start: 0.4, End: 0.8, Speaker: &spk, SpeakerConfidence: 0.8, Channel: 1},
	}
	out, _ := Rewrite(rules, in)
	if len(out) != 1 {
		t.Fatalf("got %d words, want one merged", len(out))
	}
	if out[0].Speaker == nil || *out[0].Speaker != spk {
		t.Errorf("speaker = %v, want it carried through the merge", out[0].Speaker)
	}
	if out[0].Channel != 1 {
		t.Errorf("channel = %d, want 1", out[0].Channel)
	}
}

func TestGetFallsBackToTheBaseLanguage(t *testing.T) {
	if _, ok := Get("ru-RU"); !ok {
		t.Error("ru-RU should find the ru rules")
	}
	if _, ok := Get("de"); ok {
		t.Error("de is not registered and must not resolve")
	}
}
