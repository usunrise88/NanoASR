package diarize

import (
	"testing"

	"github.com/usunrise88/nanoasr/internal/core"
)

func word(text string, start, end float64) core.Word {
	return core.Word{Word: text, Start: start, End: end}
}

func TestAssignPicksTheLargestOverlap(t *testing.T) {
	turns := []Turn{
		{Start: 0, End: 1, Speaker: 0},
		{Start: 1, End: 2, Speaker: 1},
	}
	// Three quarters inside the second turn, so it belongs to speaker 1 even
	// though it began inside the first.
	got := Assign([]core.Word{word("да", 0.75, 1.75)}, turns)

	if got[0].Speaker != "spk_1" {
		t.Errorf("speaker = %q, want spk_1 — the larger overlap wins", got[0].Speaker)
	}
	if got[0].Confidence < 0.7 || got[0].Confidence > 0.8 {
		t.Errorf("confidence = %v, want about 0.75 — the share of the word covered", got[0].Confidence)
	}
}

// A word wholly inside one turn is as attributed as attribution gets.
func TestAssignScoresAContainedWordFully(t *testing.T) {
	got := Assign([]core.Word{word("да", 0.2, 0.4)}, []Turn{{Start: 0, End: 1, Speaker: 0}})
	if got[0].Confidence != 1 {
		t.Errorf("confidence = %v, want 1", got[0].Confidence)
	}
}

// The fallback is the case SPEC §5.7 asks to be marked, and it used to be
// invisible: Assign returned a label and no way to tell it was a guess.
func TestAssignMarksTheNearestTurnFallback(t *testing.T) {
	turns := []Turn{{Start: 10, End: 11, Speaker: 3}}
	got := Assign([]core.Word{word("да", 0, 0.5)}, turns)

	if got[0].Speaker != "spk_3" {
		t.Errorf("speaker = %q, want the nearest turn spk_3", got[0].Speaker)
	}
	if got[0].Confidence != FallbackConfidence {
		t.Errorf("confidence = %v, want FallbackConfidence %v for a word no turn covered",
			got[0].Confidence, FallbackConfidence)
	}
}

// A model can report a word with no duration. Dividing by it would produce
// +Inf, which then serialises as invalid JSON.
func TestAssignSurvivesAZeroLengthWord(t *testing.T) {
	got := Assign([]core.Word{word("да", 1, 1)}, []Turn{{Start: 0, End: 2, Speaker: 0}})
	if got[0].Confidence != FallbackConfidence {
		t.Errorf("confidence = %v, want the fallback for a word with no duration", got[0].Confidence)
	}
}

func TestAssignWithNoTurnsAttributesNothing(t *testing.T) {
	got := Assign([]core.Word{word("да", 0, 1)}, nil)
	if got[0].Speaker != "" {
		t.Errorf("speaker = %q, want empty when there are no turns at all", got[0].Speaker)
	}
}

func spk(s string) *string { return &s }

func segmentWith(start, end float64, words ...core.Word) core.Segment {
	return core.Segment{Start: start, End: end, Words: words}
}

func TestSplitCutsAtTheSpeakerChange(t *testing.T) {
	seg := segmentWith(0, 4,
		core.Word{Word: "алло", Start: 0.1, End: 0.6, Speaker: spk("spk_0")},
		core.Word{Word: "слушаю", Start: 0.6, End: 1.4, Speaker: spk("spk_0")},
		core.Word{Word: "здравствуйте", Start: 2.0, End: 2.9, Speaker: spk("spk_1")},
		core.Word{Word: "это", Start: 2.9, End: 3.5, Speaker: spk("spk_1")},
	)

	got := Split([]core.Segment{seg})
	if len(got) != 2 {
		t.Fatalf("got %d segments, want one per speaker", len(got))
	}
	if *got[0].Speaker != "spk_0" || *got[1].Speaker != "spk_1" {
		t.Errorf("speakers = %q, %q", *got[0].Speaker, *got[1].Speaker)
	}
	// The outer edges stay the parent's: the transcript must not drift away
	// from the audio it was cut from.
	if got[0].Start != 0 {
		t.Errorf("first child starts at %v, want the parent's 0", got[0].Start)
	}
	if got[1].End != 4 {
		t.Errorf("last child ends at %v, want the parent's 4", got[1].End)
	}
	if got[0].Text != "алло слушаю" || got[1].Text != "здравствуйте это" {
		t.Errorf("texts = %q / %q", got[0].Text, got[1].Text)
	}
	for i, s := range got {
		if s.ID != i {
			t.Errorf("segment %d has ID %d", i, s.ID)
		}
	}
}

// One stray word must not shred a sentence. This rule is the difference
// between a transcript and confetti.
func TestSplitAbsorbsARunTooShortToBeATurn(t *testing.T) {
	seg := segmentWith(0, 3,
		core.Word{Word: "я", Start: 0.1, End: 0.3, Speaker: spk("spk_0")},
		core.Word{Word: "хотел", Start: 0.3, End: 0.7, Speaker: spk("spk_0")},
		core.Word{Word: "бы", Start: 0.7, End: 0.8, Speaker: spk("spk_1")}, // one short word
		core.Word{Word: "уточнить", Start: 0.8, End: 1.6, Speaker: spk("spk_0")},
		core.Word{Word: "детали", Start: 1.6, End: 2.4, Speaker: spk("spk_0")},
	)

	got := Split([]core.Segment{seg})
	if len(got) != 1 {
		t.Fatalf("got %d segments, want 1: a single short word is not a turn", len(got))
	}
	if *got[0].Speaker != "spk_0" {
		t.Errorf("speaker = %q, want spk_0", *got[0].Speaker)
	}
}

func TestSplitLeavesASingleSpeakerWhole(t *testing.T) {
	seg := segmentWith(0, 2,
		core.Word{Word: "да", Start: 0.1, End: 0.5, Speaker: spk("spk_0")},
		core.Word{Word: "конечно", Start: 0.5, End: 1.2, Speaker: spk("spk_0")},
	)
	got := Split([]core.Segment{seg})
	if len(got) != 1 {
		t.Fatalf("got %d segments, want 1", len(got))
	}
	if got[0].Speaker == nil || *got[0].Speaker != "spk_0" {
		t.Error("a single-speaker segment must still claim its speaker")
	}
}

// Two segments must never share a speaker pointer: a later rewrite of one would
// silently change the other.
func TestSplitDoesNotShareSpeakerPointers(t *testing.T) {
	seg := segmentWith(0, 4,
		core.Word{Word: "алло", Start: 0.1, End: 0.9, Speaker: spk("spk_0")},
		core.Word{Word: "слушаю", Start: 0.9, End: 1.6, Speaker: spk("spk_0")},
		core.Word{Word: "здравствуйте", Start: 2.0, End: 2.9, Speaker: spk("spk_1")},
		core.Word{Word: "это", Start: 2.9, End: 3.6, Speaker: spk("spk_1")},
	)
	got := Split([]core.Segment{seg})
	if got[0].Speaker == got[1].Speaker {
		t.Fatal("two segments share one speaker pointer")
	}
}

func TestSpeakersSummarisesTheTranscript(t *testing.T) {
	segs := []core.Segment{
		{Start: 0, End: 2, Speaker: spk("spk_0")},
		{Start: 2, End: 5, Speaker: spk("spk_1")},
		{Start: 5, End: 6, Speaker: spk("spk_0")},
	}
	got := Speakers(segs)

	if len(got) != 2 {
		t.Fatalf("got %d speakers, want 2", len(got))
	}
	if got[0].ID != "spk_0" || got[0].TotalSpeech != 3 || got[0].Segments != 2 {
		t.Errorf("spk_0 = %+v, want 3s over 2 segments", got[0])
	}
	if got[1].ID != "spk_1" || got[1].TotalSpeech != 3 || got[1].Segments != 1 {
		t.Errorf("spk_1 = %+v, want 3s over 1 segment", got[1])
	}
}

func TestSpeakersOfAnUnattributedTranscriptIsEmpty(t *testing.T) {
	if got := Speakers([]core.Segment{{Start: 0, End: 1}}); got != nil {
		t.Errorf("Speakers = %v, want nil when nothing is attributed", got)
	}
}

// Apply refuses a mapping that does not line up, because labelling a transcript
// from a stale attribution is worse than not labelling it.
func TestApplyRejectsAMismatchedMapping(t *testing.T) {
	segs := []core.Segment{segmentWith(0, 1, word("да", 0, 1))}
	if Apply(segs, []Attribution{{Speaker: "spk_0"}, {Speaker: "spk_1"}}) {
		t.Error("Apply accepted more attributions than there are words")
	}
	if segs[0].Words[0].Speaker != nil {
		t.Error("Apply wrote a speaker despite refusing the mapping")
	}
}

func TestApplyWritesSpeakerAndConfidence(t *testing.T) {
	segs := []core.Segment{segmentWith(0, 1, word("да", 0, 1))}
	if !Apply(segs, []Attribution{{Speaker: "spk_2", Confidence: 0.9}}) {
		t.Fatal("Apply refused a mapping that lines up")
	}
	w := segs[0].Words[0]
	if w.Speaker == nil || *w.Speaker != "spk_2" || w.SpeakerConfidence != 0.9 {
		t.Errorf("word = %+v, want spk_2 at 0.9", w)
	}
}

// The mirror image of TestSplitAbsorbsARunTooShortToBeATurn: a stray word at
// the very start has no run before it to be absorbed into, so it used to
// survive as a segment of its own — observed in the wild as a 0.1-second spk_0
// in front of a sentence that was entirely spk_1.
func TestSplitAbsorbsAShortLeadingRun(t *testing.T) {
	seg := segmentWith(0, 3,
		core.Word{Word: "а", Start: 0.1, End: 0.2, Speaker: spk("spk_0")}, // the stray
		core.Word{Word: "мы", Start: 0.4, End: 0.7, Speaker: spk("spk_1")},
		core.Word{Word: "уже", Start: 0.7, End: 1.1, Speaker: spk("spk_1")},
		core.Word{Word: "отправили", Start: 1.1, End: 2.0, Speaker: spk("spk_1")},
	)

	got := Split([]core.Segment{seg})
	if len(got) != 1 {
		t.Fatalf("got %d segments, want 1: one leading word is not a turn", len(got))
	}
	if got[0].Speaker == nil || *got[0].Speaker != "spk_1" {
		t.Errorf("speaker = %v, want spk_1 — the speaker holding the segment's time",
			derefSpeaker(got[0].Speaker))
	}
}

// A segment claims the speaker who holds most of its time, not the speaker of
// whichever word happens to come first. Taking the first word would hand this
// whole sentence to spk_0 on the strength of a single 0.1-second mistake.
func TestSplitClaimsTheDominantSpeaker(t *testing.T) {
	seg := segmentWith(0, 3,
		core.Word{Word: "и", Start: 0.0, End: 0.1, Speaker: spk("spk_0")},
		core.Word{Word: "поэтому", Start: 0.1, End: 1.0, Speaker: spk("spk_1")},
		core.Word{Word: "мы", Start: 1.0, End: 1.4, Speaker: spk("spk_1")},
		core.Word{Word: "решили", Start: 1.4, End: 2.5, Speaker: spk("spk_1")},
	)
	got := Split([]core.Segment{seg})
	if len(got) != 1 {
		t.Fatalf("got %d segments, want 1", len(got))
	}
	if got[0].Speaker == nil || *got[0].Speaker != "spk_1" {
		t.Errorf("speaker = %v, want spk_1", derefSpeaker(got[0].Speaker))
	}
}

// Two genuine turns still separate: absorbing must not become merging.
func TestSplitStillCutsWhenBothRunsAreTurns(t *testing.T) {
	seg := segmentWith(0, 5,
		core.Word{Word: "добрый", Start: 0.1, End: 0.6, Speaker: spk("spk_0")},
		core.Word{Word: "день", Start: 0.6, End: 1.2, Speaker: spk("spk_0")},
		core.Word{Word: "здравствуйте", Start: 1.5, End: 2.3, Speaker: spk("spk_1")},
		core.Word{Word: "слушаю", Start: 2.3, End: 3.0, Speaker: spk("spk_1")},
	)
	got := Split([]core.Segment{seg})
	if len(got) != 2 {
		t.Fatalf("got %d segments, want 2", len(got))
	}
	if *got[0].Speaker != "spk_0" || *got[1].Speaker != "spk_1" {
		t.Errorf("speakers = %v/%v, want spk_0/spk_1",
			derefSpeaker(got[0].Speaker), derefSpeaker(got[1].Speaker))
	}
}

func derefSpeaker(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
