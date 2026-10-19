package isolation

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/diarize"
	"github.com/usunrise88/nanoasr/internal/tse"
	"github.com/usunrise88/nanoasr/internal/tse/voxlock"
)

// turns is a diarizer that finds what it is told to.
type turns struct {
	out   []diarize.Turn
	block chan struct{} // when set, Process waits on it
}

func (d *turns) Process(ctx context.Context, _ audio.PCM, _ int) ([]diarize.Turn, error) {
	if d.block != nil {
		select {
		case <-d.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return d.out, nil
}
func (d *turns) Close() error { return nil }

// halves is an engine that "isolates" by halving the input, so the tests can
// tell its output from its input, and samples every voice it can from where
// it first speaks.
type halves struct {
	mu        sync.Mutex
	extracted []tse.Sample
	others    [][][]float32
	keep      [][]tse.Span
}

func (e *halves) Samples(_ context.Context, x []float32, sp []tse.Speaker) ([]tse.Voice, error) {
	var out []tse.Voice
	for i, s := range sp {
		if s.Spans[0].End-s.Spans[0].Start < 1 {
			out = append(out, tse.Voice{Speaker: s, Err: tse.ErrTooLittleSpeech{Clean: 0.5}})
			continue
		}
		a := make([]float32, voxlock.EnrollSamples)
		for j := range a {
			a[j] = float32(i+1) / 10
		}
		out = append(out, tse.Voice{Speaker: s, Sample: tse.Sample{
			Kind: tse.SampleWindow, Start: s.Spans[0].Start, Audio: a, Similarity: 0.9,
			Embedding: []float32{float32(i + 1)},
		}})
	}
	return out, nil
}

func (e *halves) Extract(_ context.Context, x []float32, s tse.Sample, others [][]float32, keep []tse.Span, progress func(float64)) ([]float32, error) {
	e.mu.Lock()
	e.extracted = append(e.extracted, s)
	e.others = append(e.others, others)
	e.keep = append(e.keep, keep)
	e.mu.Unlock()
	y := make([]float32, len(x))
	for i := range x {
		y[i] = x[i] / 2
	}
	progress(0.5)
	return y, nil
}

func (e *halves) Embed([]float32) ([]float32, error) { return []float32{9}, nil }

func wavBytes(seconds float64) []byte {
	x := make([]float32, int(seconds*SampleRate))
	for i := range x {
		x[i] = 0.25
	}
	var b bytes.Buffer
	_ = audio.WriteWAV(&b, SampleRate, x)
	return b.Bytes()
}

type upload struct{ b []byte }

func (u upload) Open() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(u.b)), nil }
func (u upload) Filename() string             { return "call.mp3" }
func (u upload) Size() int64                  { return int64(len(u.b)) }
func (u upload) Close() error                 { return nil }

type fixture struct {
	svc     *Service
	dir     string
	eng     *halves
	diar    *turns
	clock   *time.Time
	submits []core.Request
}

func newFixture(t *testing.T, opt func(*Options)) *fixture {
	t.Helper()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	f := &fixture{
		dir:   filepath.Join(t.TempDir(), "isolations"),
		eng:   &halves{},
		diar:  &turns{out: []diarize.Turn{{Start: 0, End: 4, Speaker: 3}, {Start: 4, End: 9, Speaker: 1}, {Start: 9, End: 9.5, Speaker: 7}}},
		clock: &now,
	}
	o := Options{
		Dir:      f.dir,
		TTL:      time.Hour,
		Diarizer: f.diar,
		Engine:   func(context.Context) (Engine, error) { return f.eng, nil },
		Decode: func(ctx context.Context, r io.Reader) ([]float32, error) {
			pcm, err := audio.NewWAVDecoder().Decode(ctx, r, audio.Options{TargetSampleRate: SampleRate})
			if err != nil {
				return nil, err
			}
			return pcm[0].Samples, nil
		},
		Submit: func(_ context.Context, req core.Request) (*core.Job, error) {
			f.submits = append(f.submits, req)
			return &core.Job{ID: "job_1", Status: core.JobQueued}, nil
		},
		Now: func() time.Time { return *f.clock },
	}
	if opt != nil {
		opt(&o)
	}
	svc, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	svc.Start(context.Background())
	t.Cleanup(svc.Close)
	f.svc = svc
	return f
}

// wait watches until the isolation stops working, and returns where it ended.
func (f *fixture) wait(t *testing.T, ctx context.Context, id string) core.Isolation {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ch, err := f.svc.Watch(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var last core.Isolation
	for ev := range ch {
		last = ev.Isolation
	}
	if last.Status.Busy() {
		t.Fatalf("watch ended with the isolation %s", last.Status)
	}
	return last
}

func TestTheWholeConversation(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	iso, err := f.svc.Create(ctx, core.IsolationRequest{Audio: upload{wavBytes(10)}, Source: core.SourceUI})
	if err != nil {
		t.Fatal(err)
	}
	if iso.Status != core.IsolationQueued || iso.Filename != "call.mp3" {
		t.Fatalf("created as %+v", iso)
	}

	got := f.wait(t, ctx, iso.ID)
	if got.Status != core.IsolationAwaitingSelection {
		t.Fatalf("after analysis: %s %+v", got.Status, got.Error)
	}
	if got.Duration != 10 {
		t.Errorf("duration %g", got.Duration)
	}
	// Numbered by first appearance, listed by how much they speak.
	if len(got.Voices) != 3 || got.Voices[0].Speaker != "spk_1" || got.Voices[1].Speaker != "spk_0" {
		t.Fatalf("voices %+v", got.Voices)
	}
	if got.Voices[0].TotalSpeech != 5 || got.Voices[0].Sample == nil || got.Voices[0].Sample.Start != 4 {
		t.Errorf("spk_1 = %+v", got.Voices[0])
	}
	if short := got.Voices[2]; short.Sample != nil || short.Reason == "" {
		t.Errorf("a voice too short to sample: %+v", short)
	}
	// The upload is decoded and gone; what is kept is the PCM.
	if _, err := os.Stat(filepath.Join(f.dir, iso.ID, uploadFile)); !os.IsNotExist(err) {
		t.Errorf("the upload is still there: %v", err)
	}

	// The sample is the 3 seconds the engine chose, as WAV.
	rc, err := f.svc.Sample(ctx, iso.ID, "spk_1")
	if err != nil {
		t.Fatal(err)
	}
	pcm, err := audio.NewWAVDecoder().Decode(ctx, rc, audio.Options{TargetSampleRate: SampleRate})
	_ = rc.Close()
	if err != nil || len(pcm[0].Samples) != voxlock.EnrollSamples {
		t.Fatalf("sample: %v", err)
	}
	if _, err := f.svc.Sample(ctx, iso.ID, "spk_2"); core.AsError(err).Code != core.CodeInvalidRequest {
		t.Errorf("the sample of a voice without one: %v", err)
	}

	// No result before an extraction.
	if _, err := f.svc.Result(ctx, iso.ID, core.AudioWAV); core.AsError(err).Code != core.CodeIsolationBusy {
		t.Errorf("result before extraction: %v", err)
	}

	if _, err := f.svc.Extract(ctx, iso.ID, core.IsolationSelection{Speaker: "spk_1"}); err != nil {
		t.Fatal(err)
	}
	got = f.wait(t, ctx, iso.ID)
	if got.Status != core.IsolationDone || got.Result == nil || got.Result.Speaker != "spk_1" {
		t.Fatalf("after extraction: %s %+v %+v", got.Status, got.Result, got.Error)
	}
	if len(got.Formats) != 1 || got.Formats[0] != core.AudioWAV {
		t.Errorf("formats %v without an encoder", got.Formats)
	}
	// The engine was told the chosen voice's own turns and every other
	// voice that has a sample.
	if keep := f.eng.keep[0]; len(keep) != 1 || keep[0] != (tse.Span{Start: 4, End: 9}) {
		t.Errorf("keep %v", keep)
	}
	if others := f.eng.others[0]; len(others) != 1 || others[0][0] != 1 {
		t.Errorf("others %v", others)
	}

	rc, err = f.svc.Result(ctx, iso.ID, core.AudioWAV)
	if err != nil {
		t.Fatal(err)
	}
	pcm, err = audio.NewWAVDecoder().Decode(ctx, rc, audio.Options{TargetSampleRate: SampleRate})
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if y := pcm[0].Samples; len(y) != 10*SampleRate || y[100] < 0.12 || y[100] > 0.13 {
		t.Errorf("result: %d samples, %g", len(y), y[100])
	}
	if _, err := f.svc.Result(ctx, iso.ID, core.AudioMP3); core.AsError(err).Code != core.CodeCapabilityUnavailable {
		t.Errorf("mp3 without an encoder: %v", err)
	}

	job, err := f.svc.Transcribe(ctx, iso.ID, core.Request{ModelID: "gigaam", Language: "ru"})
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != "job_1" || len(f.submits) != 1 {
		t.Fatalf("job %+v, %d submits", job, len(f.submits))
	}
	sub := f.submits[0]
	if sub.ModelID != "gigaam" || sub.Audio.Filename() != "call-spk_1.wav" {
		t.Errorf("submitted %+v as %s", sub, sub.Audio.Filename())
	}
	r, _ := sub.Audio.Open()
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if int64(len(b)) != sub.Audio.Size() || string(b[:4]) != "RIFF" {
		t.Errorf("the job's audio is %d bytes (%q), Size says %d", len(b), b[:4], sub.Audio.Size())
	}
	if now, _ := f.svc.Isolation(ctx, iso.ID); now.Result.JobID != "job_1" {
		t.Errorf("job id not recorded: %+v", now.Result)
	}

	// A sample placed by hand.
	start := 1.5
	if _, err := f.svc.Extract(ctx, iso.ID, core.IsolationSelection{Speaker: "spk_0", SampleStart: &start}); err != nil {
		t.Fatal(err)
	}
	f.wait(t, ctx, iso.ID)
	if s := f.eng.extracted[1]; s.Start != 1.5 || s.Embedding[0] != 9 {
		t.Errorf("placed sample: %+v", s)
	}

	if err := f.svc.Delete(ctx, iso.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Isolation(ctx, iso.ID); core.AsError(err).Code != core.CodeIsolationNotFound {
		t.Errorf("after delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, iso.ID)); !os.IsNotExist(err) {
		t.Errorf("the audio outlived the delete: %v", err)
	}
}

func TestSomeoneElsesIsolationIsNotFound(t *testing.T) {
	f := newFixture(t, nil)
	alice := core.WithCaller(context.Background(), core.Caller{KeyID: "alice"})
	bob := core.WithCaller(context.Background(), core.Caller{KeyID: "bob"})
	admin := core.WithCaller(context.Background(), core.Caller{KeyID: "root", Admin: true})

	iso, err := f.svc.Create(alice, core.IsolationRequest{Audio: upload{wavBytes(10)}})
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"get":    func() error { _, err := f.svc.Isolation(bob, iso.ID); return err },
		"watch":  func() error { _, err := f.svc.Watch(bob, iso.ID, 0); return err },
		"sample": func() error { _, err := f.svc.Sample(bob, iso.ID, "spk_0"); return err },
		"extract": func() error {
			_, err := f.svc.Extract(bob, iso.ID, core.IsolationSelection{Speaker: "spk_0"})
			return err
		},
		"result": func() error { _, err := f.svc.Result(bob, iso.ID, core.AudioWAV); return err },
		"delete": func() error { return f.svc.Delete(bob, iso.ID) },
	} {
		if err := call(); core.AsError(err).Code != core.CodeIsolationNotFound {
			t.Errorf("%s by another key: %v", name, err)
		}
	}
	if _, err := f.svc.Isolation(admin, iso.ID); err != nil {
		t.Errorf("an administrator: %v", err)
	}
}

func TestNothingCanBeChosenWhileTheVoicesAreFound(t *testing.T) {
	f := newFixture(t, nil)
	f.diar.block = make(chan struct{})
	ctx := context.Background()
	iso, err := f.svc.Create(ctx, core.IsolationRequest{Audio: upload{wavBytes(10)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Extract(ctx, iso.ID, core.IsolationSelection{Speaker: "spk_0"}); core.AsError(err).Code != core.CodeIsolationBusy {
		t.Errorf("extract while analysing: %v", err)
	}
	close(f.diar.block)
	f.wait(t, ctx, iso.ID)
	if _, err := f.svc.Extract(ctx, iso.ID, core.IsolationSelection{Speaker: "spk_9"}); core.AsError(err).Code != core.CodeInvalidRequest {
		t.Errorf("an unknown voice: %v", err)
	}
	if _, err := f.svc.Extract(ctx, iso.ID, core.IsolationSelection{Speaker: "spk_2"}); core.AsError(err).Code != core.CodeInvalidRequest {
		t.Errorf("a voice with no sample and no sample_start: %v", err)
	}
	late := 8.0
	if _, err := f.svc.Extract(ctx, iso.ID, core.IsolationSelection{Speaker: "spk_0", SampleStart: &late}); core.AsError(err).Code != core.CodeInvalidRequest {
		t.Errorf("a sample running past the end: %v", err)
	}
}

// Deleting an isolation mid-analysis stops the work rather than letting it
// run on for nobody.
func TestDeleteStopsTheWork(t *testing.T) {
	f := newFixture(t, nil)
	// Released only after the delete: until then, only cancellation ends it.
	f.diar.block = make(chan struct{})
	ctx := context.Background()
	iso, err := f.svc.Create(ctx, core.IsolationRequest{Audio: upload{wavBytes(10)}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, _ := f.svc.Isolation(ctx, iso.ID)
		if v.Stage == "diarize" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never reached diarization")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := f.svc.Delete(ctx, iso.ID); err != nil {
		t.Fatal(err)
	}
	// The worker is free again: a second isolation gets analysed.
	close(f.diar.block)
	iso2, err := f.svc.Create(ctx, core.IsolationRequest{Audio: upload{wavBytes(10)}})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.wait(t, ctx, iso2.ID); got.Status != core.IsolationAwaitingSelection {
		t.Errorf("the next isolation: %s", got.Status)
	}
}

func TestIdleIsolationsExpire(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.TTL = time.Minute })
	ctx := context.Background()
	iso, err := f.svc.Create(ctx, core.IsolationRequest{Audio: upload{wavBytes(10)}})
	if err != nil {
		t.Fatal(err)
	}
	f.wait(t, ctx, iso.ID)

	*f.clock = f.clock.Add(59 * time.Second)
	f.svc.expire()
	if _, err := f.svc.Isolation(ctx, iso.ID); err != nil {
		t.Fatalf("expired early: %v", err)
	}
	// Fetching the sample is not a change; only what moves the isolation on
	// keeps it alive, so a forgotten page polling it does not keep it forever.
	*f.clock = f.clock.Add(2 * time.Second)
	f.svc.expire()
	if _, err := f.svc.Isolation(ctx, iso.ID); core.AsError(err).Code != core.CodeIsolationNotFound {
		t.Fatalf("still there after its time: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, iso.ID)); !os.IsNotExist(err) {
		t.Errorf("its audio outlived it: %v", err)
	}
}

func TestTheNumberOfIsolationsIsBounded(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.MaxIsolations = 1 })
	ctx := context.Background()
	if _, err := f.svc.Create(ctx, core.IsolationRequest{Audio: upload{wavBytes(10)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Create(ctx, core.IsolationRequest{Audio: upload{wavBytes(10)}}); core.AsError(err).Code != core.CodeQueueFull {
		t.Errorf("a second isolation over the limit: %v", err)
	}
}

func TestLeftoversAreClearedAtStart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "isolations")
	if err := os.MkdirAll(filepath.Join(dir, "iso_old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "iso_old", sourceFile), []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	newFixture(t, func(o *Options) { o.Dir = dir })
	if _, err := os.Stat(filepath.Join(dir, "iso_old")); !os.IsNotExist(err) {
		t.Errorf("a previous run's audio survived the start: %v", err)
	}
}

func TestAnalysisFailureIsReported(t *testing.T) {
	f := newFixture(t, nil)
	f.diar.out = nil
	ctx := context.Background()
	iso, err := f.svc.Create(ctx, core.IsolationRequest{Audio: upload{wavBytes(10)}})
	if err != nil {
		t.Fatal(err)
	}
	got := f.wait(t, ctx, iso.ID)
	if got.Status != core.IsolationFailed || got.Error == nil || got.Error.Code != core.CodeInvalidRequest {
		t.Fatalf("no voices: %s %+v", got.Status, got.Error)
	}
	if _, err := f.svc.Extract(ctx, iso.ID, core.IsolationSelection{Speaker: "spk_0"}); core.AsError(err).Code != core.CodeIsolationBusy {
		t.Errorf("extract after a failed analysis: %v", err)
	}
}

func TestResultName(t *testing.T) {
	for in, want := range map[string]string{
		"call.mp3":       "call-spk_1",
		"a.b.wav":        "a.b-spk_1",
		"":               "audio-spk_1",
		"../../etc/x.gz": "x-spk_1",
	} {
		if got := resultName(in, "spk_1"); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
