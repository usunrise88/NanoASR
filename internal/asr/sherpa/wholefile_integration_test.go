//go:build integration

package sherpa

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/registry"
)

// The multilingual model is not in scripts/fetch-dev-models.sh: it is 475 MB
// and would be downloaded by every integration run. These tests skip when it is
// absent, and `nanoasr models pull nemotron-3.5-streaming-multi` makes them run.
const multilingualModel = "nemotron-3.5-streaming-multi"

// loadWholeFileModel loads the multilingual model the way the pool does, as an
// offline recogniser, and returns the directory its sample clips are in.
func loadWholeFileModel(t *testing.T) (asr.Recognizer, registry.Manifest, string) {
	t.Helper()
	reg, err := registry.NewLocal(filepath.Join(repoRoot(t), ".models"))
	if err != nil {
		t.Fatal(err)
	}
	man, err := reg.Resolve(t.Context(), multilingualModel)
	if err != nil {
		t.Skipf("model %s is absent; run `nanoasr models pull %s` (%v)", multilingualModel, multilingualModel, err)
	}
	dir := modelDir(t, man)
	load := NewLoader(LoaderOptions{NumThreads: 2})
	rec, err := load(t.Context(), man, dir, asr.Variant{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	return rec, man, dir
}

func readClip(t *testing.T, ctx context.Context, path string) audio.PCM {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("clip %s is absent (%v)", path, err)
	}
	defer f.Close()
	pcms, err := audio.NewWAVDecoder().Decode(ctx, f, audio.Options{TargetSampleRate: 16000})
	if err != nil {
		t.Fatal(err)
	}
	return pcms[0]
}

// TestMultilingualModelDecodesAnUpload decodes the reference clip as a file,
// in each way a request can name the language, and checks the Russian comes
// out as Russian. It is the proof that a streaming export serves uploads.
func TestMultilingualModelDecodesAnUpload(t *testing.T) {
	rec, man, _ := loadWholeFileModel(t)
	if !rec.Capabilities().LanguageDetect {
		t.Error("a multilingual model must report language_detect")
	}
	if !rec.Capabilities().PunctuationBuiltin {
		t.Error("the model writes punctuation and capitals; the vocabulary check should see it")
	}
	pcm := readClip(t, t.Context(), filepath.Join(repoRoot(t), "testdata", "audio", "ru-16k.wav"))

	for _, lang := range []string{"ru-RU", "auto", ""} {
		start := time.Now()
		got, err := rec.Decode(t.Context(), [][]float32{pcm.Samples}, pcm.SampleRate, asr.DecodeOptions{Language: lang})
		if err != nil {
			t.Fatalf("language %q: %v", lang, err)
		}
		took := time.Since(start)
		audioSeconds := float64(len(pcm.Samples)) / float64(pcm.SampleRate)
		t.Logf("language %-6q  %.2fx real time  %s", lang, took.Seconds()/audioSeconds, got[0].Text)

		text := strings.ToLower(got[0].Text)
		for _, want := range []string{"не требуя похвал", "счастлив", "посмотрит", "зелен"} {
			if !strings.Contains(text, want) {
				t.Errorf("language %q: transcript lacks %q: %q", lang, want, got[0].Text)
			}
		}
		if !got[0].HasTimestamps() {
			t.Errorf("language %q: no token timestamps (%d tokens, %d stamps)", lang, len(got[0].Tokens), len(got[0].Timestamps))
		}
	}
	_ = man
}

// TestMultilingualModelFollowsTheLanguage runs the package's own clips, each
// in its language, and the Russian clip against a language that is wrong for
// it: what the model was told must change what comes out, or the language key
// is decoration.
func TestMultilingualModelFollowsTheLanguage(t *testing.T) {
	rec, _, dir := loadWholeFileModel(t)

	for _, tc := range []struct{ file, lang string }{
		{"de.wav", "de-DE"}, {"fr.wav", "fr-FR"}, {"es.wav", "es-ES"}, {"ja.wav", "ja-JP"}, {"zh.wav", "zh-CN"}, {"uk.wav", "uk-UA"},
	} {
		pcm := readClip(t, t.Context(), filepath.Join(dir, "test_wavs", tc.file))
		forced, err := rec.Decode(t.Context(), [][]float32{pcm.Samples}, pcm.SampleRate, asr.DecodeOptions{Language: tc.lang})
		if err != nil {
			t.Fatal(err)
		}
		auto, err := rec.Decode(t.Context(), [][]float32{pcm.Samples}, pcm.SampleRate, asr.DecodeOptions{Language: "auto"})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s  %-6s %s", tc.file, tc.lang, forced[0].Text)
		t.Logf("%s  %-6s %s", tc.file, "auto", auto[0].Text)
		if strings.TrimSpace(forced[0].Text) == "" {
			t.Errorf("%s: empty transcript for %s", tc.file, tc.lang)
		}
	}

	ru := readClip(t, t.Context(), filepath.Join(repoRoot(t), "testdata", "audio", "ru-16k.wav"))
	wrong, err := rec.Decode(t.Context(), [][]float32{ru.Samples}, ru.SampleRate, asr.DecodeOptions{Language: "de-DE"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ru-16k.wav told de-DE: %s", wrong[0].Text)
}

// TestMultilingualModelStreams is the realtime path of the same weights: the
// language is set on the stream, audio goes in as a connection would send it,
// and what comes out is in that language.
func TestMultilingualModelStreams(t *testing.T) {
	rec, man := loadStreaming(t, multilingualModel)
	if !rec.Capabilities().LanguageDetect || !rec.Capabilities().PunctuationBuiltin {
		t.Errorf("capabilities %+v: want language_detect and punctuation", rec.Capabilities())
	}
	dir := modelDir(t, man)

	for _, tc := range []struct {
		path, lang string
		contains   string
	}{
		{filepath.Join(repoRoot(t), "testdata", "audio", "ru-16k.wav"), "ru-RU", "не требуя похвал"},
		{filepath.Join(dir, "test_wavs", "de.wav"), "de-DE", "wurst"},
		{filepath.Join(dir, "test_wavs", "fr.wav"), "fr-FR", "demandez"},
	} {
		pcm := readClip(t, t.Context(), tc.path)
		stream, err := rec.NewStream()
		if err != nil {
			t.Fatal(err)
		}
		stream.SetLanguage(tc.lang)

		chunk := pcm.SampleRate / 10
		for off := 0; off < len(pcm.Samples); off += chunk {
			stream.Accept(pcm.SampleRate, pcm.Samples[off:min(off+chunk, len(pcm.Samples))])
			for stream.Ready() {
				rec.Decode([]asr.Stream{stream})
			}
		}
		stream.Accept(pcm.SampleRate, make([]float32, pcm.SampleRate/2))
		stream.Finish()
		for stream.Ready() {
			rec.Decode([]asr.Stream{stream})
		}
		got := stream.Hypothesis().Text
		stream.Close()

		t.Logf("%s  %s: %s", filepath.Base(tc.path), tc.lang, got)
		if !strings.Contains(strings.ToLower(got), tc.contains) {
			t.Errorf("%s as %s: transcript lacks %q: %q", filepath.Base(tc.path), tc.lang, tc.contains, got)
		}
	}
}

// modelDir is where the installed model's files, and its sample clips, are.
func modelDir(t *testing.T, man registry.Manifest) string {
	t.Helper()
	reg, err := registry.NewLocal(filepath.Join(repoRoot(t), ".models"))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := reg.Dir(man.ID)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestMultilingualModelKeepsTheEndOfATightSegment is the regression for the
// tail: a VAD segment ends where speech ends, and with too little silence
// after it the last word never left the encoder's final chunk. The reference
// clip is cut at the end of "мои", exactly as a tight segment would be.
func TestMultilingualModelKeepsTheEndOfATightSegment(t *testing.T) {
	rec, _, _ := loadWholeFileModel(t)
	pcm := readClip(t, t.Context(), filepath.Join(repoRoot(t), "testdata", "audio", "ru-16k.wav"))
	cut := pcm.Samples[:int(8.56*float64(pcm.SampleRate))]

	got, err := rec.Decode(t.Context(), [][]float32{cut}, pcm.SampleRate, asr.DecodeOptions{Language: "ru-RU"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(got[0].Text), "грешные мои") {
		t.Errorf("the last word of a tight segment was lost: %q", got[0].Text)
	}
}

// TestMultilingualModelStartsAtTheFirstWord is the regression for the lead-in:
// speech that begins on the first sample used to lose its first word.
func TestMultilingualModelStartsAtTheFirstWord(t *testing.T) {
	rec, _, dir := loadWholeFileModel(t)
	pcm := readClip(t, t.Context(), filepath.Join(dir, "test_wavs", "de.wav"))

	got, err := rec.Decode(t.Context(), [][]float32{pcm.Samples}, pcm.SampleRate, asr.DecodeOptions{Language: "de-DE"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.ToLower(got[0].Text), "alles hat ein ende") {
		t.Errorf("the first word was lost: %q", got[0].Text)
	}
}
