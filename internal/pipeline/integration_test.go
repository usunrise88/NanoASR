//go:build integration

// Integration tests for the M1 vertical slice.
//
// They need real weights and real audio, so they are behind a build tag:
//
//	./scripts/fetch-dev-models.sh
//	./scripts/fetch-testdata.sh
//	go test -tags integration ./internal/pipeline/...
//
// TestM1Report prints the numbers M1 has to report and does not assert on
// them, because a threshold on someone else's hardware is noise.
package pipeline

import (
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/usunrise88/nanoasr/internal/asr/sherpa"
	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/pool"
	"github.com/usunrise88/nanoasr/internal/registry"
	"github.com/usunrise88/nanoasr/internal/vad"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/golden from this run")

const (
	testModel    = "gigaam-v3-ctc-punct-ru"
	testVADModel = "silero-vad-v5"
)

// --- fixtures ---------------------------------------------------------------

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

type fileSource struct{ path string }

func (f fileSource) Open() (io.ReadCloser, error) { return os.Open(f.path) }
func (f fileSource) Filename() string             { return filepath.Base(f.path) }
func (f fileSource) Size() int64 {
	info, err := os.Stat(f.path)
	if err != nil {
		return 0
	}
	return info.Size()
}
func (f fileSource) Close() error { return nil }

// newStack builds the real pipeline: real registry, real weights, real VAD,
// real decoders. Only the transport is missing.
// newRegistryFor opens the development models directory.
func newRegistryFor(t *testing.T) *registry.Local {
	t.Helper()
	reg, err := registry.NewLocal(filepath.Join(repoRoot(t), ".models"))
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func newStack(t *testing.T) *Pipeline {
	t.Helper()
	root := repoRoot(t)

	// Models install as id@revision directories, so presence is a registry
	// question rather than a path one.
	reg := newRegistryFor(t)
	_ = root
	if _, err := reg.Resolve(t.Context(), testModel); err != nil {
		t.Skipf("models are absent; run ./scripts/fetch-dev-models.sh (%v)", err)
	}

	models := pool.New(reg,
		sherpa.NewLoader(sherpa.LoaderOptions{Provider: "cpu", NumThreads: 2}),
		pool.Options{MaxResidentModels: 3, MaxModelRSSMB: 8192, AcquireTimeout: 2 * time.Minute})
	t.Cleanup(func() { _ = models.Close() })

	vadManifest, err := reg.Resolve(t.Context(), testVADModel)
	if err != nil {
		t.Skipf("VAD model is absent; run ./scripts/fetch-dev-models.sh (%v)", err)
	}
	vadDir, err := reg.Dir(testVADModel)
	if err != nil {
		t.Fatal(err)
	}
	vadPath, err := vadManifest.FilePath(vadDir, "model")
	if err != nil {
		t.Fatal(err)
	}

	segmenter, err := vad.NewPool(vad.Config{
		Family: vadManifest.Family, ModelPath: vadPath, SampleRate: 16000,
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = segmenter.Close() })

	decoder := audio.NewRouter(
		audio.NewWAVDecoder(),
		audio.NewFFmpegDecoder("ffmpeg", 60*time.Second),
	)

	return New(decoder, segmenter, models, pool.NewGovernor(4), Options{
		DefaultModel: testModel,
		MaxDuration:  30 * time.Minute,
		NumThreads:   2,
	})
}

func audioPath(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(repoRoot(t), "testdata", "audio", name)
	if _, err := os.Stat(p); err != nil {
		t.Skipf("test audio is absent; run ./scripts/fetch-testdata.sh (%v)", err)
	}
	return p
}

func transcribe(t *testing.T, p *Pipeline, name string) *core.Result {
	t.Helper()
	res, err := p.Transcribe(t.Context(), core.Request{Audio: fileSource{path: audioPath(t, name)}})
	if err != nil {
		t.Fatalf("transcribing %s: %v", name, err)
	}
	return res
}

// transcribeWithModel runs a specific model over an absolute path, for the
// cross-family comparisons.
func transcribeWithModel(t *testing.T, p *Pipeline, model, path string) *core.Result {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Skipf("audio is absent: %v", err)
	}
	res, err := p.Transcribe(t.Context(), core.Request{
		Audio: fileSource{path: path}, ModelID: model,
	})
	if err != nil {
		t.Fatalf("transcribing %s with %s: %v", path, model, err)
	}
	return res
}

// --- golden -----------------------------------------------------------------

type golden struct {
	Model string      `json:"model"`
	Text  string      `json:"text"`
	Words []core.Word `json:"words"`
}

func goldenPath(t *testing.T) string {
	return filepath.Join(repoRoot(t), "testdata", "golden", "ru-16k.json")
}

func loadGolden(t *testing.T) golden {
	t.Helper()
	b, err := os.ReadFile(goldenPath(t))
	if err != nil {
		t.Fatalf("golden transcript is missing; regenerate with -update-golden (%v)", err)
	}
	var g golden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// --- tests ------------------------------------------------------------------

// The end-to-end check: a real file through the real stack produces the
// expected transcript with usable word timings.
func TestIntegrationTranscribesReferenceClip(t *testing.T) {
	p := newStack(t)
	got := transcribe(t, p, "ru-16k.wav")

	if *updateGolden {
		writeGolden(t, got)
		t.Skip("golden transcript rewritten")
	}
	want := loadGolden(t)

	if wer := wordErrorRate(want.Text, got.Text); wer > 0.05 {
		t.Errorf("word error rate %.1f%% against the golden transcript\n got:  %s\n want: %s",
			wer*100, got.Text, want.Text)
	}
	if got.TimestampSource != core.TimestampToken {
		t.Errorf("timestamp_source = %q, want token: this model does produce token timings",
			got.TimestampSource)
	}

	words := got.Words()
	if len(words) == 0 {
		t.Fatal("no words returned")
	}
	assertWordInvariants(t, words, got.Duration)

	// Timings must be close to the golden run, not merely well-formed: a
	// regression that shifts every word by a frame would otherwise pass.
	if drift := medianStartDrift(want.Words, words); drift > 0.05 {
		t.Errorf("median word-start drift from the golden run is %.0f ms, want under 50 ms", drift*1000)
	}
}

func TestIntegrationSilenceRegionsMatchSpeech(t *testing.T) {
	p := newStack(t)
	got := transcribe(t, p, "ru-16k.wav")

	if len(got.Silence) == 0 {
		t.Fatal("no silence regions: the player has nothing to paint")
	}
	for i, s := range got.Silence {
		if s.Start < 0 || s.End > got.Duration+1e-6 || s.Start >= s.End {
			t.Errorf("silence %d = [%.3f,%.3f] outside [0,%.3f]", i, s.Start, s.End, got.Duration)
		}
		// No word may sit inside a silence region.
		for _, w := range got.Words() {
			if w.Start >= s.Start && w.End <= s.End {
				t.Errorf("word %q at [%.3f,%.3f] lies inside silence [%.3f,%.3f]",
					w.Word, w.Start, w.End, s.Start, s.End)
			}
		}
	}
}

// Every decode path must reach the same transcript. A divergence here means a
// decoder, the resampler or the channel handling is wrong, not the model.
func TestIntegrationDecodePathsAgree(t *testing.T) {
	p := newStack(t)
	reference := transcribe(t, p, "ru-16k.wav").Text

	for _, name := range []string{"ru-8k-alaw.wav", "ru-8k-ulaw.wav", "ru-16k.mp3", "ru-16k.opus"} {
		t.Run(name, func(t *testing.T) {
			got := transcribe(t, p, name)
			assertWordInvariants(t, got.Words(), got.Duration)

			// 10% allows for narrowband loss and codec priming; a broken
			// decoder produces gibberish, not a 10% edit distance.
			if wer := wordErrorRate(reference, got.Text); wer > 0.10 {
				t.Errorf("word error rate %.1f%% against the 16 kHz run\n got: %s\n ref: %s",
					wer*100, got.Text, reference)
			}
		})
	}
}

// TestM1Report prints what M1 promised to measure. It asserts nothing: the
// numbers depend on the host, and a threshold here would fail on a laptop and
// pass on a server while telling nobody anything.
func TestM1Report(t *testing.T) {
	p := newStack(t)

	files := []string{"ru-16k.wav", "ru-8k-alaw.wav", "ru-8k-ulaw.wav", "ru-16k.mp3", "ru-16k.opus"}
	results := map[string]*core.Result{}

	// Warm the model before measuring: the first transcription would otherwise
	// carry several hundred megabytes of model load in its RTF and make one
	// arbitrary row of the table meaningless.
	transcribe(t, p, files[0])

	t.Log("")
	t.Log("=== M1 report ===")
	t.Logf("%-16s %8s %7s %7s %7s %7s %6s %7s", "file", "dur", "rtf", "decode", "vad", "asr", "words", "speech")

	for _, name := range files {
		res := transcribe(t, p, name)
		results[name] = res
		t.Logf("%-16s %7.2fs %7.3f %6dms %6dms %6dms %6d %6.0f%%",
			name, res.Duration, res.Stats.RTF,
			res.Stats.StagesMS["decode"], res.Stats.StagesMS["vad"], res.Stats.StagesMS["asr"],
			len(res.Words()), res.Stats.SpeechRatio*100)
	}

	// The telephony question: how far do word boundaries move when the same
	// speech arrives narrowband instead of wideband?
	base := results["ru-16k.wav"].Words()
	t.Log("")
	t.Log("word-boundary drift against the 16 kHz reference")
	t.Logf("%-16s %6s %8s %8s %8s %8s", "file", "words", "median", "mean", "p95", "max")

	for _, name := range files[1:] {
		d := startDrifts(base, results[name].Words())
		if len(d) == 0 {
			t.Logf("%-16s %6s  no comparable words", name, "-")
			continue
		}
		t.Logf("%-16s %6d %7.0fms %7.0fms %7.0fms %7.0fms",
			name, len(d), percentile(d, 0.5)*1000, mean(d)*1000,
			percentile(d, 0.95)*1000, percentile(d, 1.0)*1000)
	}

	reportFamilies(t, p)

	t.Log("")
	t.Log("Absolute timing accuracy is not measured here: the reference clip has")
	t.Log("no hand-aligned word boundaries. These numbers are the narrowband")
	t.Log("penalty, which is the part that does not depend on ground truth.")
}

// reportFamilies compares the two Russian models, which share acoustics and
// differ only in decoder. This is the evidence behind choosing a default model
// (SPEC §19.2 #5): the same audio, the same front end, two decoders.
func reportFamilies(t *testing.T, p *Pipeline) {
	const rnnt = "gigaam-v2-rnnt-ru"
	if _, err := newRegistryFor(t).Resolve(t.Context(), rnnt); err != nil {
		t.Logf("\n%s is not installed; skipping the family comparison", rnnt)
		return
	}

	wav := audioPath(t, "ru-16k.wav")
	ctcResult := transcribeWithModel(t, p, testModel, wav)
	rnntResult := transcribeWithModel(t, p, rnnt, wav)

	t.Log("")
	t.Log("CTC against RNNT on identical audio")
	t.Logf("%-20s %8s %7s %6s %11s", "model", "rtf", "asr", "words", "confidence")
	for _, r := range []struct {
		name string
		res  *core.Result
	}{{testModel, ctcResult}, {rnnt, rnntResult}} {
		conf := "absent"
		if hasAnyConfidence(r.res.Words()) {
			conf = "present"
		}
		t.Logf("%-20s %8.3f %5dms %6d %11s",
			r.name, r.res.Stats.RTF, r.res.Stats.StagesMS["asr"], len(r.res.Words()), conf)
	}
	t.Logf("word error rate between them: %.1f%%",
		wordErrorRate(ctcResult.Text, rnntResult.Text)*100)
}

func hasAnyConfidence(ws []core.Word) bool {
	for _, w := range ws {
		if w.Confidence > 0 {
			return true
		}
	}
	return false
}

// --- helpers ----------------------------------------------------------------

func assertWordInvariants(t *testing.T, ws []core.Word, duration float64) {
	t.Helper()
	for i, w := range ws {
		if w.Word == "" {
			t.Errorf("word %d is empty", i)
		}
		if w.Start < 0 || w.End > duration+1e-6 || w.Start > w.End {
			t.Errorf("word %d %q has span [%.3f,%.3f], outside [0,%.3f]",
				i, w.Word, w.Start, w.End, duration)
		}
		if strings.TrimSpace(w.Word) != w.Word {
			t.Errorf("word %d %q carries surrounding whitespace", i, w.Word)
		}
		if i > 0 && ws[i-1].End > w.Start+1e-9 {
			t.Errorf("word %d overlaps its predecessor: %.3f > %.3f", i, ws[i-1].End, w.Start)
		}
	}
}

func writeGolden(t *testing.T, res *core.Result) {
	t.Helper()
	b, err := json.MarshalIndent(golden{Model: res.Model, Text: res.Text, Words: res.Words()}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goldenPath(t), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", goldenPath(t))
}

// wordErrorRate is the usual Levenshtein distance over words, normalised by
// the reference length.
func wordErrorRate(reference, hypothesis string) float64 {
	ref := normaliseForWER(reference)
	hyp := normaliseForWER(hypothesis)
	if len(ref) == 0 {
		if len(hyp) == 0 {
			return 0
		}
		return 1
	}

	prev := make([]int, len(hyp)+1)
	cur := make([]int, len(hyp)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ref); i++ {
		cur[0] = i
		for j := 1; j <= len(hyp); j++ {
			cost := 1
			if ref[i-1] == hyp[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return float64(prev[len(hyp)]) / float64(len(ref))
}

// normaliseForWER lowercases and strips punctuation before comparing.
//
// This is deliberate rather than incidental. The default model punctuates, so
// its words arrive with marks attached — mergePunctuation glues a comma to the
// word before it, by design, so that a comma is not a word with a duration.
// Counting "похвал," as a substitution for "похвал" would make the error rate a
// measure of punctuation agreement rather than of recognition, and every
// comparison here is between models or between codecs, where the question is
// whether the words are the same.
func normaliseForWER(s string) []string {
	fields := strings.Fields(strings.ToLower(s))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimFunc(f, func(r rune) bool {
			return unicode.IsPunct(r) || unicode.IsSymbol(r)
		})
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// startDrifts pairs identical words between two transcripts and reports how
// far their onsets moved.
//
// The alignment tolerates insertions, deletions and substitutions with a small
// lookahead. A plain forward scan does not: narrowband audio drops the leading
// "ни" of the first word, and a scan that skips ahead looking for it consumes
// the entire second transcript and reports nothing at all.
func startDrifts(base, other []core.Word) []float64 {
	const lookahead = 3

	var out []float64
	i, j := 0, 0
	for i < len(base) && j < len(other) {
		if sameWord(base[i].Word, other[j].Word) {
			out = append(out, abs(other[j].Start-base[i].Start))
			i++
			j++
			continue
		}
		switch {
		case findWithin(other, j+1, lookahead, base[i].Word) >= 0:
			j++ // an extra word in other
		case findWithin(base, i+1, lookahead, other[j].Word) >= 0:
			i++ // a word missing from other
		default:
			i++ // a substitution: neither side is comparable
			j++
		}
	}
	return out
}

// sameWord compares two words for alignment, ignoring case and attached
// punctuation. A punctuating model can place a comma differently on narrowband
// audio than on wideband, and treating that as a substitution would make this a
// measurement of punctuation agreement rather than of when words start.
func sameWord(a, b string) bool {
	trim := func(s string) string {
		return strings.TrimFunc(strings.ToLower(s), func(r rune) bool {
			return unicode.IsPunct(r) || unicode.IsSymbol(r)
		})
	}
	return trim(a) == trim(b)
}

// findWithin looks for word in seq[from:from+n], returning its index or -1.
func findWithin(seq []core.Word, from, n int, word string) int {
	for k := from; k < from+n && k < len(seq); k++ {
		if sameWord(seq[k].Word, word) {
			return k
		}
	}
	return -1
}

func medianStartDrift(base, other []core.Word) float64 {
	d := startDrifts(base, other)
	if len(d) == 0 {
		return 0
	}
	return percentile(d, 0.5)
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	idx := int(p * float64(len(s)-1))
	return s[idx]
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
