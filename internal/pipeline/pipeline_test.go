package pipeline

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/pool"
	"github.com/usunrise88/nanoasr/internal/registry"
	"github.com/usunrise88/nanoasr/internal/vad"
)

const testRate = 16000

// --- fakes ------------------------------------------------------------------

type fakeSource struct{ closed bool }

func (f *fakeSource) Open() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(make([]byte, 128))), nil
}
func (f *fakeSource) Filename() string { return "call.wav" }
func (f *fakeSource) Size() int64      { return 128 }
func (f *fakeSource) Close() error     { f.closed = true; return nil }

type fakeDecoder struct{ pcm audio.PCM }

func (fakeDecoder) CanDecode(audio.Format) bool { return true }
func (d fakeDecoder) Decode(context.Context, io.Reader, audio.Options) ([]audio.PCM, error) {
	return []audio.PCM{d.pcm}, nil
}

type fakeSegmenter struct{ segments []vad.Segment }

func (f fakeSegmenter) Segment(context.Context, audio.PCM) ([]vad.Segment, error) {
	return f.segments, nil
}
func (fakeSegmenter) Close() error { return nil }

type fakeRecognizer struct {
	results []asr.Recognition
	caps    core.Capabilities
	unit    string

	mu         sync.Mutex
	batchSizes []int
	next       int
	block      chan struct{}
}

func (f *fakeRecognizer) Decode(ctx context.Context, batch [][]float32, _ int) ([]asr.Recognition, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	f.batchSizes = append(f.batchSizes, len(batch))
	out := make([]asr.Recognition, 0, len(batch))
	for range batch {
		if f.next < len(f.results) {
			out = append(out, f.results[f.next])
			f.next++
		} else {
			out = append(out, asr.Recognition{})
		}
	}
	return out, nil
}

func (f *fakeRecognizer) Capabilities() core.Capabilities { return f.caps }
func (f *fakeRecognizer) ModelingUnit() string            { return f.unit }
func (f *fakeRecognizer) Close() error                    { return nil }

type fakeRegistry struct{ languages []string }

func (r fakeRegistry) Resolve(_ context.Context, id string) (registry.Manifest, error) {
	return registry.Manifest{
		ID: id, Revision: "1", Family: "nemo_ctc", SampleRate: testRate,
		ModelingUnit: "bpe", Languages: r.languages,
		Features: registry.Features{SampleRate: testRate, Dim: 64},
		Files:    map[string]string{"model": "m.onnx", "tokens": "t.txt"},
	}, nil
}
func (fakeRegistry) Local(context.Context) ([]registry.Manifest, error)   { return nil, nil }
func (fakeRegistry) Catalog(context.Context) ([]registry.Manifest, error) { return nil, nil }
func (fakeRegistry) Ensure(context.Context, string) (string, error)       { return "/models", nil }
func (fakeRegistry) Fetch(context.Context, string) (<-chan core.DownloadProgress, error) {
	return nil, nil
}
func (fakeRegistry) Dir(string) (string, error) { return "/models", nil }

// --- harness ----------------------------------------------------------------

type harness struct {
	pipeline *Pipeline
	rec      *fakeRecognizer
}

func newHarness(t *testing.T, pcm audio.PCM, segs []vad.Segment, rec *fakeRecognizer, opt Options) *harness {
	t.Helper()

	models := pool.New(fakeRegistry{languages: []string{"ru"}},
		func(context.Context, registry.Manifest, string, asr.Variant) (asr.Recognizer, error) { return rec, nil },
		pool.Options{MaxResidentModels: 2, MaxModelRSSMB: 4096})
	t.Cleanup(func() { _ = models.Close() })

	if opt.DefaultModel == "" {
		opt.DefaultModel = "test-model"
	}
	p := New(audio.NewRouter(fakeDecoder{pcm: pcm}), fakeSegmenter{segments: segs},
		models, pool.NewGovernor(4), opt)

	return &harness{pipeline: p, rec: rec}
}

// silence builds a PCM buffer of the given duration.
func silence(seconds float64) audio.PCM {
	return audio.PCM{Samples: make([]float32, int(seconds*testRate)), SampleRate: testRate, SourceChannels: 1}
}

func segment(startSec, lenSec float64) vad.Segment {
	return vad.Segment{
		StartSample: int(startSec * testRate),
		Samples:     make([]float32, int(lenSec*testRate)),
	}
}

func timedRecognition(text string, tokens []string, ts, dur []float32) asr.Recognition {
	return asr.Recognition{Text: text, Tokens: tokens, Timestamps: ts, Durations: dur}
}

// --- tests ------------------------------------------------------------------

func TestTranscribeAssemblesAbsoluteWordTimes(t *testing.T) {
	// Two utterances with a gap: word times must be absolute, not relative to
	// their segment, or the player seeks to the wrong place.
	segs := []vad.Segment{segment(1.0, 1.0), segment(3.0, 1.0)}
	rec := &fakeRecognizer{
		unit: "bpe",
		caps: core.Capabilities{WordTimestamps: true, Confidence: true},
		results: []asr.Recognition{
			timedRecognition("привет мир",
				[]string{"▁привет", "▁мир"}, []float32{0.10, 0.50}, []float32{0.30, 0.40}),
			timedRecognition("как дела",
				[]string{"▁как", "▁дела"}, []float32{0.05, 0.40}, []float32{0.25, 0.50}),
		},
	}

	h := newHarness(t, silence(5), segs, rec, Options{})
	got, err := h.pipeline.Transcribe(context.Background(), core.Request{Audio: &fakeSource{}})
	if err != nil {
		t.Fatal(err)
	}

	if got.Text != "привет мир как дела" {
		t.Errorf("Text = %q", got.Text)
	}
	if got.TimestampSource != core.TimestampToken {
		t.Errorf("TimestampSource = %q, want token", got.TimestampSource)
	}

	words := got.Words()
	if len(words) != 4 {
		t.Fatalf("got %d words %+v, want 4", len(words), words)
	}
	// First word of the second segment starts at 3.0 + 0.05.
	if !near(words[2].Start, 3.05) {
		t.Errorf("word %q starts at %.3f, want 3.05 — segment offset was not applied",
			words[2].Word, words[2].Start)
	}
	for i, w := range words {
		if w.Start < 0 || w.End > got.Duration || w.Start > w.End {
			t.Errorf("word %d %q has an impossible span [%.3f,%.3f]", i, w.Word, w.Start, w.End)
		}
		if i > 0 && words[i-1].End > w.Start {
			t.Errorf("word %d overlaps its predecessor", i)
		}
	}
}

func TestTranscribeReportsStats(t *testing.T) {
	segs := []vad.Segment{segment(1.0, 2.0)}
	rec := &fakeRecognizer{
		unit: "bpe", caps: core.Capabilities{WordTimestamps: true},
		results: []asr.Recognition{
			timedRecognition("да", []string{"▁да"}, []float32{0.1}, []float32{0.3}),
		},
	}

	h := newHarness(t, silence(10), segs, rec, Options{})
	got, err := h.pipeline.Transcribe(context.Background(), core.Request{Audio: &fakeSource{}})
	if err != nil {
		t.Fatal(err)
	}

	if !near(got.Stats.AudioDuration, 10) {
		t.Errorf("AudioDuration = %.3f, want 10", got.Stats.AudioDuration)
	}
	if !near(got.Stats.SpeechRatio, 0.2) {
		t.Errorf("SpeechRatio = %.3f, want 0.2", got.Stats.SpeechRatio)
	}
	if got.Stats.SegmentsTotal != 1 {
		t.Errorf("SegmentsTotal = %d, want 1", got.Stats.SegmentsTotal)
	}
	for _, stage := range []string{"decode", "vad", "asr"} {
		if _, ok := got.Stats.StagesMS[stage]; !ok {
			t.Errorf("stage %q is missing from StagesMS %v", stage, got.Stats.StagesMS)
		}
	}
	// Silence around the single utterance: before and after.
	if len(got.Silence) != 2 {
		t.Errorf("got %d silence regions %+v, want 2", len(got.Silence), got.Silence)
	}
}

// A model without token timings must say so rather than invent word spans.
func TestTranscribeFallsBackToSegmentTimings(t *testing.T) {
	segs := []vad.Segment{segment(0.5, 1.5)}
	rec := &fakeRecognizer{
		unit:    "bpe",
		caps:    core.Capabilities{},
		results: []asr.Recognition{{Text: "текст без таймингов"}},
	}

	h := newHarness(t, silence(3), segs, rec, Options{})
	got, err := h.pipeline.Transcribe(context.Background(), core.Request{Audio: &fakeSource{}})
	if err != nil {
		t.Fatal(err)
	}

	if got.TimestampSource != core.TimestampSegment {
		t.Fatalf("TimestampSource = %q, want segment", got.TimestampSource)
	}
	if !hasWarning(got.Warnings, "word_timestamps_unavailable") {
		t.Errorf("warnings %+v should include word_timestamps_unavailable", got.Warnings)
	}
	words := got.Words()
	if len(words) != 1 || !near(words[0].Start, 0.5) || !near(words[0].End, 2.0) {
		t.Errorf("fallback word = %+v, want one word spanning the segment", words)
	}
}

func TestTranscribeWarnsAboutUnimplementedOptions(t *testing.T) {
	segs := []vad.Segment{segment(0, 1)}
	rec := &fakeRecognizer{
		unit: "bpe", caps: core.Capabilities{WordTimestamps: true},
		results: []asr.Recognition{
			timedRecognition("да", []string{"▁да"}, []float32{0}, []float32{0.2}),
		},
	}

	h := newHarness(t, silence(2), segs, rec, Options{})
	req := core.Request{
		Audio: &fakeSource{}, Diarize: true, Punctuate: true, ITN: true,
		Hotwords: []string{"ромашка"}, Language: "en",
	}
	got, err := h.pipeline.Transcribe(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	for _, code := range []string{
		"diarization_unavailable", "punctuation_unavailable",
		"itn_unavailable", "hotwords_unavailable", "language_mismatch",
	} {
		if !hasWarning(got.Warnings, code) {
			t.Errorf("warnings %+v should include %s", got.Warnings, code)
		}
	}
}

// Strict mode is what a client uses when a silently degraded answer is worse
// than no answer.
func TestTranscribeStrictModeRejectsDegradedResults(t *testing.T) {
	segs := []vad.Segment{segment(0, 1)}
	rec := &fakeRecognizer{unit: "bpe", results: []asr.Recognition{{Text: "нет таймингов"}}}

	h := newHarness(t, silence(2), segs, rec, Options{})
	_, err := h.pipeline.Transcribe(context.Background(),
		core.Request{Audio: &fakeSource{}, Strict: true})

	var e *core.Error
	if !errors.As(err, &e) || e.Code != core.CodeCapabilityUnavailable {
		t.Fatalf("got %v, want capability_unavailable", err)
	}
}

func TestTranscribeBatchesSegments(t *testing.T) {
	// Ten one-second segments with a batch cap of four: 4, 4, 2.
	var segs []vad.Segment
	var results []asr.Recognition
	for i := range 10 {
		segs = append(segs, segment(float64(i), 1))
		results = append(results, timedRecognition("слово",
			[]string{"▁слово"}, []float32{0.1}, []float32{0.5}))
	}
	rec := &fakeRecognizer{unit: "bpe", caps: core.Capabilities{WordTimestamps: true}, results: results}

	h := newHarness(t, silence(10), segs, rec, Options{BatchMaxSize: 4, BatchMaxSeconds: 60})
	if _, err := h.pipeline.Transcribe(context.Background(), core.Request{Audio: &fakeSource{}}); err != nil {
		t.Fatal(err)
	}

	want := []int{4, 4, 2}
	if len(rec.batchSizes) != len(want) {
		t.Fatalf("batches = %v, want %v", rec.batchSizes, want)
	}
	for i, n := range want {
		if rec.batchSizes[i] != n {
			t.Errorf("batches = %v, want %v", rec.batchSizes, want)
			break
		}
	}
}

func TestTranscribeBatchesRespectTotalDuration(t *testing.T) {
	// Segments of 30 s each with a 60 s cap: two per batch even though the
	// count limit would allow eight.
	var segs []vad.Segment
	var results []asr.Recognition
	for i := range 4 {
		segs = append(segs, segment(float64(i)*30, 30))
		results = append(results, asr.Recognition{Text: "x"})
	}
	rec := &fakeRecognizer{unit: "bpe", results: results}

	h := newHarness(t, silence(120), segs, rec, Options{BatchMaxSize: 8, BatchMaxSeconds: 60})
	if _, err := h.pipeline.Transcribe(context.Background(), core.Request{Audio: &fakeSource{}}); err != nil {
		t.Fatal(err)
	}
	for _, n := range rec.batchSizes {
		if n > 2 {
			t.Fatalf("batches = %v; a batch exceeded the 60 s budget", rec.batchSizes)
		}
	}
}

// Cancelling has to stop the decode, not merely stop waiting for it: a client
// that disconnects should stop costing CPU.
func TestTranscribeCancellationStopsDecoding(t *testing.T) {
	segs := []vad.Segment{segment(0, 1), segment(2, 1)}
	rec := &fakeRecognizer{
		unit:    "bpe",
		results: []asr.Recognition{{Text: "a"}, {Text: "b"}},
		block:   make(chan struct{}),
	}

	h := newHarness(t, silence(4), segs, rec, Options{BatchMaxSize: 1})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := h.pipeline.Transcribe(ctx, core.Request{Audio: &fakeSource{}})
		done <- err
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Transcribe did not return after its context was cancelled")
	}
}

func TestTranscribeRequiresAModel(t *testing.T) {
	h := newHarness(t, silence(1), nil, &fakeRecognizer{unit: "bpe"}, Options{DefaultModel: " "})
	h.pipeline.opt.DefaultModel = ""

	_, err := h.pipeline.Transcribe(context.Background(), core.Request{Audio: &fakeSource{}})
	var e *core.Error
	if !errors.As(err, &e) || e.Code != core.CodeInvalidRequest {
		t.Fatalf("got %v, want invalid_request", err)
	}
	if !strings.Contains(err.Error(), "default_model") {
		t.Errorf("error should point at the missing configuration, got %q", err)
	}
}

func TestTranscribeReportsNoSpeech(t *testing.T) {
	h := newHarness(t, silence(5), nil, &fakeRecognizer{unit: "bpe"}, Options{})
	got, err := h.pipeline.Transcribe(context.Background(), core.Request{Audio: &fakeSource{}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "" || len(got.Segments) != 0 {
		t.Errorf("expected an empty transcript, got %q", got.Text)
	}
	if !hasWarning(got.Warnings, "no_speech_detected") {
		t.Errorf("warnings %+v should include no_speech_detected", got.Warnings)
	}
}

func TestAsyncSurfaceIsHonestlyUnimplemented(t *testing.T) {
	h := newHarness(t, silence(1), nil, &fakeRecognizer{unit: "bpe"}, Options{})
	ctx := context.Background()

	if _, err := h.pipeline.Submit(ctx, core.Request{}); err == nil {
		t.Error("Submit should report not_implemented rather than silently succeeding")
	}
	if _, err := h.pipeline.Job(ctx, "x"); err == nil {
		t.Error("Job should report not_implemented")
	}
	if err := h.pipeline.Cancel(ctx, "x"); err == nil {
		t.Error("Cancel should report not_implemented")
	}
}

func hasWarning(ws []core.Warning, code string) bool {
	for _, w := range ws {
		if w.Code == code {
			return true
		}
	}
	return false
}

func near(a, b float64) bool {
	d := a - b
	return d < 1e-3 && d > -1e-3
}

// A model that punctuates itself answers punctuate=true by punctuating. Saying
// "unavailable" anyway would train people to ignore the warnings field.
func TestTranscribeDoesNotWarnWhenTheModelPunctuates(t *testing.T) {
	segs := []vad.Segment{segment(0, 1)}
	rec := &fakeRecognizer{
		unit: "bpe",
		caps: core.Capabilities{WordTimestamps: true, PunctuationBuiltin: true},
		results: []asr.Recognition{
			timedRecognition("Да.", []string{"▁Да."}, []float32{0}, []float32{0.2}),
		},
	}

	h := newHarness(t, silence(2), segs, rec, Options{})
	got, err := h.pipeline.Transcribe(context.Background(),
		core.Request{Audio: &fakeSource{}, Punctuate: true})
	if err != nil {
		t.Fatal(err)
	}
	if hasWarning(got.Warnings, "punctuation_unavailable") {
		t.Errorf("warnings %+v claim punctuation is unavailable from a model that punctuates",
			got.Warnings)
	}
}

// The same request against a model that cannot punctuate must still say so,
// and must still fail strict mode.
func TestTranscribeWarnsWhenTheModelCannotPunctuate(t *testing.T) {
	segs := []vad.Segment{segment(0, 1)}
	rec := &fakeRecognizer{
		unit: "bpe", caps: core.Capabilities{WordTimestamps: true},
		results: []asr.Recognition{
			timedRecognition("да", []string{"▁да"}, []float32{0}, []float32{0.2}),
		},
	}

	h := newHarness(t, silence(2), segs, rec, Options{})
	got, err := h.pipeline.Transcribe(context.Background(),
		core.Request{Audio: &fakeSource{}, Punctuate: true})
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(got.Warnings, "punctuation_unavailable") {
		t.Errorf("warnings %+v should say punctuation was asked for and not delivered", got.Warnings)
	}
}
