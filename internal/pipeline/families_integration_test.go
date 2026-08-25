//go:build integration

package pipeline

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/usunrise88/nanoasr/internal/core"
)

const (
	enModel = "zipformer-small-en"
	// charModel is named rather than reached through the default, because the
	// default is a subword model now and the character branch of word assembly
	// still has to be exercised by something.
	charModel = "gigaam-v2-ctc-ru"
	rnntModel = "gigaam-v3-rnnt-punct-ru"
)

// Both transducer entries in the catalog exist to answer questions the CTC
// model cannot: whether the transducer mapping works on real weights, whether
// its confidence claim is true, and whether word assembly handles a
// SentencePiece vocabulary rather than only a character one.
func TestIntegrationTransducerFamily(t *testing.T) {
	p := newStack(t)

	// The same generation on both sides. Comparing across generations would
	// measure how much the model improved, which is not a property of the
	// transducer mapping and not something a test can hold to a threshold.
	ctc := transcribe(t, p, "ru-16k.wav")
	rnnt := transcribeWithModel(t, p, rnntModel, audioPath(t, "ru-16k.wav"))

	assertWordInvariants(t, rnnt.Words(), rnnt.Duration)

	// Same acoustics, different decoder: the transcripts should agree closely.
	if wer := wordErrorRate(ctc.Text, rnnt.Text); wer > 0.10 {
		t.Errorf("CTC and RNNT disagree by %.1f%%\n ctc:  %s\n rnnt: %s",
			wer*100, ctc.Text, rnnt.Text)
	}

	// The capability claim, checked by fact. CTC's identical claim was removed
	// in M1 once measurement showed ys_log_probs comes back empty.
	if !hasConfidence(rnnt.Words()) {
		t.Error("transducer words carry no confidence, but the family claims it")
	}
	if hasConfidence(ctc.Words()) {
		t.Error("CTC words carry confidence, which the family no longer claims")
	}
}

// The English model is the only catalog entry with a SentencePiece vocabulary.
// Its tokens arrive as " AFTER", " E", "AR", "LY" — the marker rendered as a
// leading space — and matching only "▁" once merged an entire sentence into a
// single word spanning six seconds.
func TestIntegrationSentencePieceVocabulary(t *testing.T) {
	p := newStack(t)

	wav := filepath.Join(repoRoot(t), ".models", "zipformer-small-en@2023-06-26", "test_wavs", "0.wav")
	res := transcribeWithModel(t, p, enModel, wav)

	words := res.Words()
	if len(words) < 10 {
		t.Fatalf("got %d words for a full sentence: %q", len(words), res.Text)
	}
	assertWordInvariants(t, words, res.Duration)

	for _, w := range words {
		if len(w.Word) > 30 {
			t.Errorf("word %q is a merged phrase, not a word", w.Word)
		}
	}
	if !hasConfidence(words) {
		t.Error("transducer words carry no confidence")
	}
}

// The token shapes are a contract with sherpa-onnx that nothing else states.
// A release that changed them would otherwise show up as subtly wrong word
// boundaries rather than as a failure.
func TestIntegrationTokenShapesAreUnchanged(t *testing.T) {
	p := newStack(t)

	ctc := transcribeWithModel(t, p, charModel, audioPath(t, "ru-16k.wav"))
	if got := ctc.TimestampSource; got != core.TimestampToken {
		t.Errorf("CTC timestamp_source = %q, want token", got)
	}
	// A character vocabulary must not produce one word per character.
	for _, w := range ctc.Words() {
		if len([]rune(w.Word)) < 2 && w.Word != "я" && w.Word != "с" && w.Word != "у" {
			t.Errorf("suspiciously short word %q: the character vocabulary may be "+
				"assembling one word per character", w.Word)
		}
	}

	// The Russian subword model is the default, and it is the one whose output
	// most users see. Same question, different vocabulary: a broken boundary
	// rule shows up here as one enormous word rather than as many tiny ones.
	punct := transcribe(t, p, "ru-16k.wav")
	if n := len(punct.Words()); n < 5 {
		t.Errorf("the default subword model produced %d words for an 11s clip: %q",
			n, punct.Text)
	}
	if !strings.ContainsAny(punct.Text, ".,?!") {
		t.Errorf("the default model is meant to punctuate, but wrote none: %q", punct.Text)
	}

	wav := filepath.Join(repoRoot(t), ".models", "zipformer-small-en@2023-06-26", "test_wavs", "0.wav")
	en := transcribeWithModel(t, p, enModel, wav)
	// A SentencePiece vocabulary must not produce one word for the sentence.
	if len(en.Words()) == 1 {
		t.Errorf("the whole sentence came back as one word: %q", en.Words()[0].Word)
	}
}

func hasConfidence(ws []core.Word) bool {
	for _, w := range ws {
		if w.Confidence > 0 {
			return true
		}
	}
	return false
}
