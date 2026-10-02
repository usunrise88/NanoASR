//go:build integration

// Integration tests for the streaming recogniser. They need real weights:
//
//	./scripts/fetch-dev-models.sh
//	./scripts/fetch-testdata.sh
//	go test -tags integration ./internal/asr/sherpa/ -v
package sherpa

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/registry"
)

// The streaming models in the catalog, with what each must get right on the
// reference clip. The expected fragments are words the offline default also
// produces, chosen at the start and after the longest pause — the two places a
// streaming model is most likely to differ.
var streamingModels = []struct {
	id       string
	rate     int // what the client sends, not what the model wants
	contains []string
}{
	{id: "t-one-ctc-ru", rate: 16000, contains: []string{"не требуя похвал", "лукоморья"}},
	{id: "streaming-zipformer-small-ru", rate: 16000, contains: []string{"не требуя похвал", "лукоморья"}},
}

const streamingModel = "t-one-ctc-ru"

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// loadStreaming opens the development models directory and loads the streaming
// model from it, skipping when it has not been fetched.
func loadStreaming(t *testing.T, id string) (*Online, registry.Manifest) {
	t.Helper()
	reg, err := registry.NewLocal(filepath.Join(repoRoot(t), ".models"))
	if err != nil {
		t.Fatal(err)
	}
	man, err := reg.Resolve(t.Context(), id)
	if err != nil {
		t.Skipf("model %s is absent; run ./scripts/fetch-dev-models.sh (%v)", id, err)
	}
	dir, err := reg.Dir(id)
	if err != nil {
		t.Fatal(err)
	}

	rec, err := NewOnline(t.Context(), man, dir, OnlineOptions{
		NumThreads:              2,
		EnableEndpoint:          true,
		Rule1MinTrailingSilence: 2.4,
		Rule2MinTrailingSilence: 1.2,
		Rule3MinUtteranceLength: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	return rec, man
}

// readPCM decodes one of the test clips to mono float32.
func readPCM(t *testing.T, name string, rate int) audio.PCM {
	t.Helper()
	path := filepath.Join(repoRoot(t), "testdata", "audio", name)
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("test audio is absent; run ./scripts/fetch-testdata.sh (%v)", err)
	}
	defer f.Close()

	pcms, err := audio.NewWAVDecoder().Decode(t.Context(), f, audio.Options{TargetSampleRate: rate})
	if err != nil {
		t.Fatal(err)
	}
	return pcms[0]
}

// TestStreamingTranscribesTheReferenceClip feeds the clip through the streaming
// recogniser the way a connection would — in chunks, decoding whatever is ready
// — and checks that what comes out is the same Russian the offline model
// produces, in lower case and without punctuation.
//
// It also reports the two numbers that decide whether realtime is viable on a
// given machine: the real-time factor of the decode, and how long after the
// audio ended the final transcript arrived.
func TestStreamingTranscribesTheReferenceClip(t *testing.T) {
	for _, tc := range streamingModels {
		t.Run(tc.id, func(t *testing.T) { streamOneClip(t, tc.id, tc.rate, tc.contains) })
	}
}

func streamOneClip(t *testing.T, id string, rate int, contains []string) {
	rec, man := loadStreaming(t, id)

	// 16 kHz is what a browser sends. Against the 8 kHz telephony model that
	// means a resampler inside AcceptWaveform, which is the path to exercise.
	pcm := readPCM(t, "ru-16k.wav", rate)
	stream, err := rec.NewStream()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	const chunkMS = 100
	chunk := pcm.SampleRate * chunkMS / 1000

	var utterances []string
	start := time.Now()
	for off := 0; off < len(pcm.Samples); off += chunk {
		end := min(off+chunk, len(pcm.Samples))
		stream.Accept(pcm.SampleRate, pcm.Samples[off:end])
		for stream.Ready() {
			rec.Decode([]asr.Stream{stream})
		}
		if stream.Endpoint() {
			if text := strings.TrimSpace(stream.Hypothesis().Text); text != "" {
				utterances = append(utterances, text)
			}
			stream.Reset()
		}
	}

	// The tail: once the audio is over, the remaining frames still decode.
	audioEnded := time.Now()
	stream.Finish()
	for stream.Ready() {
		rec.Decode([]asr.Stream{stream})
	}
	if text := strings.TrimSpace(stream.Hypothesis().Text); text != "" {
		utterances = append(utterances, text)
	}
	elapsed := time.Since(start)

	full := strings.Join(utterances, " ")
	t.Logf("model     %s (%d Hz, %s)", man.Key(), rec.SampleRate(), man.Family)
	t.Logf("audio     %.2fs", pcm.Duration())
	t.Logf("decode    %.2fs, rtf %.3f", elapsed.Seconds(), elapsed.Seconds()/pcm.Duration())
	t.Logf("tail      %.0fms after the audio ended", time.Since(audioEnded).Seconds()*1000)
	t.Logf("rss       %d MB", rssMB())
	t.Logf("utterances %d", len(utterances))
	for i, u := range utterances {
		t.Logf("  [%d] %s", i, u)
	}

	if full == "" {
		t.Fatal("the streaming recogniser produced no text at all")
	}
	for _, want := range contains {
		if !strings.Contains(full, want) {
			t.Errorf("transcript does not contain %q:\n%s", want, full)
		}
	}
	// This model's vocabulary has no capitals and no punctuation, and the
	// catalog says so. A transcript with either means the entry is wrong.
	if strings.ContainsAny(full, ".,?!") {
		t.Errorf("transcript carries punctuation, which this vocabulary cannot produce:\n%s", full)
	}
	if rec.Capabilities().PunctuationBuiltin {
		t.Error("PunctuationBuiltin is true for a vocabulary with no punctuation in it")
	}
}

// TestStreamingDecodesSeveralStreamsInOneBatch is the property the realtime
// engine is built on: one recogniser, many connections, decoded together.
func TestStreamingDecodesSeveralStreamsInOneBatch(t *testing.T) {
	rec, _ := loadStreaming(t, streamingModel)
	pcm := readPCM(t, "ru-16k.wav", 16000)

	const n = 3
	streams := make([]asr.Stream, 0, n)
	for range n {
		s, err := rec.NewStream()
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		streams = append(streams, s)
	}

	// Each stream gets the same audio from a different offset, so a batch that
	// mixed their state would show up as identical or empty transcripts.
	for i, s := range streams {
		s.Accept(pcm.SampleRate, pcm.Samples[i*pcm.SampleRate:])
		s.Finish()
	}
	for {
		ready := make([]asr.Stream, 0, n)
		for _, s := range streams {
			if s.Ready() {
				ready = append(ready, s)
			}
		}
		if len(ready) == 0 {
			break
		}
		rec.Decode(ready)
	}

	seen := make(map[string]bool)
	for i, s := range streams {
		text := strings.TrimSpace(s.Hypothesis().Text)
		if text == "" {
			t.Errorf("stream %d produced nothing", i)
			continue
		}
		t.Logf("stream %d: %.60s...", i, text)
		if seen[text] {
			t.Errorf("stream %d produced a transcript identical to an earlier one, "+
				"so the streams are sharing state", i)
		}
		seen[text] = true
	}
}

// rssMB reads the process's resident set, to report what a loaded streaming
// model costs. The catalog states an approximate figure and this is where it
// comes from.
func rssMB() int {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		var kb int
		if _, err := fmt.Sscanf(line, "VmRSS: %d kB", &kb); err != nil {
			return 0
		}
		return kb / 1024
	}
	return 0
}
