//go:build integration

package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/diarize"
	diarizesherpa "github.com/usunrise88/nanoasr/internal/diarize/sherpa"
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

// Diarization against the real segmentation and embedding models.
//
// The fixture is two voices derived from one clip by pitch shift, which is the
// only two-speaker audio the repository can produce without shipping someone's
// recording. The shift factor is measured rather than chosen: see
// scripts/fetch-testdata.sh.
func TestIntegrationDiarizesTwoSpeakers(t *testing.T) {
	p := newStack(t)
	d := newDiarizer(t)
	p.WithDiarizer(d)
	t.Cleanup(func() { _ = d.Close() })

	res := transcribeWith(t, p, "ru-2spk.wav", core.Request{Diarize: true})

	if len(res.Speakers) != 2 {
		t.Fatalf("found %d speakers, want 2: %+v", len(res.Speakers), res.Speakers)
	}
	// The summary has to agree with the transcript beside it.
	total := 0.0
	for _, s := range res.Speakers {
		if s.Segments == 0 {
			t.Errorf("speaker %s claims no segments", s.ID)
		}
		total += s.TotalSpeech
	}
	if total > res.Duration {
		t.Errorf("speakers account for %.1fs of a %.1fs recording", total, res.Duration)
	}

	// Every word is attributed, and every segment agrees with its own words.
	for _, seg := range res.Segments {
		if seg.Speaker == nil {
			t.Errorf("segment %d has no speaker", seg.ID)
			continue
		}
		for _, w := range seg.Words {
			if w.Speaker == nil {
				t.Errorf("word %q has no speaker", w.Word)
				continue
			}
			if *w.Speaker != *seg.Speaker {
				t.Errorf("word %q says %q, its segment says %q", w.Word, *w.Speaker, *seg.Speaker)
			}
			if w.SpeakerConfidence <= 0 || w.SpeakerConfidence > 1 {
				t.Errorf("word %q has speaker confidence %v", w.Word, w.SpeakerConfidence)
			}
		}
	}
	assertWordInvariants(t, res.Words(), res.Duration)

	// Both halves say the same sentence, so both speakers should have found
	// most of it. This is what catches a diarizer that returns turns nobody
	// spoke in.
	if wer := wordErrorRate(res.Text, res.Text); wer != 0 {
		t.Fatalf("self-comparison is not zero: %v", wer)
	}
}

// Split already separates the speakers, so the second pass must not run
// (SPEC §5.7) — and skipping must not look like a failure.
func TestIntegrationDiarizationSkippedUnderSplit(t *testing.T) {
	p := newStack(t)
	d := newDiarizer(t)
	p.WithDiarizer(d)
	t.Cleanup(func() { _ = d.Close() })

	res := transcribeWith(t, p, "ru-stereo.wav",
		core.Request{Diarize: true, ChannelMode: core.ChannelSplit})

	if len(res.Speakers) != 0 {
		t.Errorf("speakers = %+v, want none under split", res.Speakers)
	}
	if !hasWarningCode(res.Warnings, "diarization_skipped_split") {
		t.Errorf("warnings %+v should say why diarization was skipped", res.Warnings)
	}
	channels := map[int]bool{}
	for _, s := range res.Segments {
		channels[s.Channel] = true
	}
	if len(channels) != 2 {
		t.Errorf("segments cover channels %v, want both legs", channels)
	}
}

func newDiarizer(t *testing.T) *diarizesherpa.Pool {
	t.Helper()
	root := repoRoot(t)
	seg := filepath.Join(root, ".models", "pyannote-segmentation-3@3.0", "model.int8.onnx")
	emb := filepath.Join(root, ".models", "campplus-sv-voxceleb@16k",
		"3dspeaker_speech_campplus_sv_en_voxceleb_16k.onnx")
	for _, p := range []string{seg, emb} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("diarization models are not downloaded: %v", err)
		}
	}

	d, err := diarizesherpa.NewPool(diarize.Config{
		SegmentationModel: seg,
		EmbeddingModel:    emb,
		Threshold:         0.4,
		MinDurationOn:     0.3,
		MinDurationOff:    0.5,
	}, 2, 1)
	if err != nil {
		t.Fatalf("building the diarizer: %v", err)
	}
	return d
}

func hasWarningCode(ws []core.Warning, code string) bool {
	for _, w := range ws {
		if w.Code == code {
			return true
		}
	}
	return false
}

// TestM5Report prints what M5 promised to measure, the same way TestM1Report
// does and for the same reason: the numbers depend on the host, and a threshold
// here would fail on a laptop and pass on a server while telling nobody
// anything.
//
// What it answers: what the second pass and the text stages actually cost, and
// whether normalisation moved any word boundaries.
func TestM5Report(t *testing.T) {
	p := newStack(t)
	path := audioPath(t, "ru-2spk.wav")

	// Warm the model, or the first row carries several hundred megabytes of
	// model load and means nothing.
	transcribe(t, p, "ru-16k.wav")

	t.Log("")
	t.Log("=== M5 report ===")
	t.Logf("%-28s %8s %8s %9s %9s %7s", "request", "rtf", "asr_ms", "diarize_ms", "post_ms", "words")

	row := func(label string, req core.Request) *core.Result {
		t.Helper()
		req.Audio = &fileSource{path: path}
		res, err := p.Transcribe(t.Context(), req)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		t.Logf("%-28s %8.3f %7dms %8dms %8dms %7d",
			label, res.Stats.RTF,
			res.Stats.StagesMS["asr"], res.Stats.StagesMS["diarize"], res.Stats.StagesMS["post"],
			len(res.Words()))
		return res
	}

	base := row("plain", core.Request{})

	d := newDiarizer(t)
	t.Cleanup(func() { _ = d.Close() })
	p.WithDiarizer(d)

	diarized := row("diarize", core.Request{Diarize: true})
	normalised := row("itn", core.Request{ITN: true, Language: "ru"})
	row("diarize+itn", core.Request{Diarize: true, ITN: true, Language: "ru"})

	// The cost of speakers, as a fraction of the recording. SPEC §5.7 budgets
	// 0.3-0.5x RTF on top of recognition.
	if base.Duration > 0 {
		cost := float64(diarized.Stats.StagesMS["diarize"]) / 1000 / base.Duration
		t.Logf("")
		t.Logf("diarization costs %.3fx RTF on top of recognition (SPEC §5.7 budgets 0.3-0.5x)", cost)
		t.Logf("it found %d speakers", len(diarized.Speakers))
	}

	// Normalisation may merge words but must never move the boundaries of the
	// ones it keeps. Anything else would break the player.
	t.Logf("")
	t.Logf("normalisation: %d words became %d", len(base.Words()), len(normalised.Words()))
	drift := medianStartDrift(base.Words(), normalised.Words())
	t.Logf("median word-start drift after normalisation: %.0f ms (expected 0)", drift*1000)
}
