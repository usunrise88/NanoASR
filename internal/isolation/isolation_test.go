package isolation

import (
	"bytes"
	"context"
	"fmt"
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

// jobs is a job queue that finishes every job at once, transcribing a track
// as one segment per turn of the voice in its file name.
type jobs struct {
	mu       sync.Mutex
	submits  []core.Request
	callers  []core.Caller
	byID     map[string]*core.Job
	canceled []string
	// hold, when set, keeps every job running until it is closed.
	hold chan struct{}
	// fail names a file whose job fails.
	fail string
	// segments says what a track's transcript is, by file name.
	segments map[string][]core.Segment
}

func (j *jobs) Submit(ctx context.Context, req core.Request) (*core.Job, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	// What a job reads is a copy: the queue spools the upload.
	r, err := req.Audio.Open()
	if err != nil {
		return nil, err
	}
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if int64(len(b)) != req.Audio.Size() || string(b[:4]) != "RIFF" {
		return nil, fmt.Errorf("the job's audio is %d bytes (%q), Size says %d", len(b), b[:4], req.Audio.Size())
	}
	j.submits = append(j.submits, req)
	j.callers = append(j.callers, core.CallerOf(ctx))
	if j.byID == nil {
		j.byID = map[string]*core.Job{}
	}
	job := &core.Job{ID: fmt.Sprintf("job_%d", len(j.submits)), Status: core.JobQueued, Filename: req.Audio.Filename()}
	j.byID[job.ID] = job
	return job, nil
}

func (j *jobs) finish(id string) core.Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	job := j.byID[id]
	if job.Status == core.JobCanceled {
		return *job
	}
	if job.Filename == j.fail {
		job.Status, job.Error = core.JobFailed, core.Errorf(core.CodeInternal, "the model fell over")
		return *job
	}
	job.Status = core.JobSucceeded
	job.Result = &core.Result{Model: "gigaam", Language: "ru", Segments: j.segments[job.Filename],
		Stats:    core.Stats{ProcessingMS: 100, StagesMS: map[string]int64{"asr": 80}},
		Warnings: []core.Warning{{Code: "w", Message: job.Filename}}}
	return *job
}

func (j *jobs) Job(_ context.Context, id string) (*core.Job, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job := *j.byID[id]
	return &job, nil
}

func (j *jobs) Watch(ctx context.Context, id string, _ int64) (<-chan core.JobEvent, error) {
	out := make(chan core.JobEvent)
	go func() {
		defer close(out)
		j.mu.Lock()
		running := *j.byID[id]
		hold := j.hold
		j.mu.Unlock()
		running.Status, running.Percent = core.JobRunning, 50
		select {
		case out <- core.JobEvent{Seq: 1, Job: running}:
		case <-ctx.Done():
			return
		}
		if hold != nil {
			select {
			case <-hold:
			case <-ctx.Done():
				return
			}
		}
		select {
		case out <- core.JobEvent{Seq: 2, Job: j.finish(id)}:
		case <-ctx.Done():
		}
	}()
	return out, nil
}

func (j *jobs) Cancel(_ context.Context, id string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.canceled = append(j.canceled, id)
	j.byID[id].Status = core.JobCanceled
	return nil
}

type fixture struct {
	svc   *Service
	dir   string
	eng   *halves
	diar  *turns
	clock *time.Time
	jobs  *jobs
}

func newFixture(t *testing.T, opt func(*Options)) *fixture {
	t.Helper()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	f := &fixture{
		dir:   filepath.Join(t.TempDir(), "isolations"),
		eng:   &halves{},
		jobs:  &jobs{},
		diar:  &turns{out: []diarize.Turn{{Start: 0, End: 4, Speaker: 3}, {Start: 4, End: 9, Speaker: 1}, {Start: 9, End: 9.5, Speaker: 7}}},
		clock: &now,
	}
	o := Options{
		Dir:           f.dir,
		TTL:           time.Hour,
		MaxIsolations: 8,
		MaxKept:       8,
		Diarizer:      f.diar,
		Engine:        func(context.Context) (Engine, error) { return f.eng, nil },
		Decode: func(ctx context.Context, r io.Reader) ([]float32, error) {
			pcm, err := audio.NewWAVDecoder().Decode(ctx, r, audio.Options{TargetSampleRate: SampleRate})
			if err != nil {
				return nil, err
			}
			return pcm[0].Samples, nil
		},
		Jobs:      f.jobs,
		AllowKeep: true,
		Now:       func() time.Time { return *f.clock },
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
	if _, err := f.svc.Result(ctx, iso.ID, "", core.AudioWAV); core.AsError(err).Code != core.CodeIsolationBusy {
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

	rc, err = f.svc.Result(ctx, iso.ID, "", core.AudioWAV)
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
	if _, err := f.svc.Result(ctx, iso.ID, "", core.AudioMP3); core.AsError(err).Code != core.CodeCapabilityUnavailable {
		t.Errorf("mp3 without an encoder: %v", err)
	}

	job, err := f.svc.Transcribe(ctx, iso.ID, "", core.Request{ModelID: "gigaam", Language: "ru"})
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != "job_1" || len(f.jobs.submits) != 1 {
		t.Fatalf("job %+v, %d submits", job, len(f.jobs.submits))
	}
	sub := f.jobs.submits[0]
	if sub.ModelID != "gigaam" || sub.Audio.Filename() != "call-spk_1.wav" {
		t.Errorf("submitted %+v as %s", sub, sub.Audio.Filename())
	}
	if now, _ := f.svc.Isolation(ctx, iso.ID); now.Result.JobID != "job_1" {
		t.Errorf("job id not recorded: %+v", now.Result)
	}

	// A sample placed by hand.
	start := 1.5
	if _, err := f.svc.Extract(ctx, iso.ID, core.IsolationSelection{Speaker: "spk_0", SampleStart: &start}); err != nil {
		t.Fatal(err)
	}
	got = f.wait(t, ctx, iso.ID)
	if s := f.eng.extracted[1]; s.Start != 1.5 || s.Embedding[0] != 9 {
		t.Errorf("placed sample: %+v", s)
	}
	// The second voice is a track beside the first, not instead of it, and
	// the result is now the voice chosen last.
	if len(got.Tracks) != 2 || got.Result == nil || got.Result.Speaker != "spk_0" ||
		got.Result.SampleStart == nil || *got.Result.SampleStart != 1.5 {
		t.Errorf("tracks %+v, result %+v", got.Tracks, got.Result)
	}
	if got.ExpiresAt == nil || got.Keep {
		t.Errorf("an isolation not kept expires: %v %v", got.ExpiresAt, got.Keep)
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
		"result": func() error { _, err := f.svc.Result(bob, iso.ID, "", core.AudioWAV); return err },
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
	f := newFixture(t, func(o *Options) { o.MaxIsolations, o.MaxKept = 1, 1 })
	ctx := context.Background()
	if _, err := f.svc.Create(ctx, core.IsolationRequest{Audio: upload{wavBytes(10)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Create(ctx, core.IsolationRequest{Audio: upload{wavBytes(10)}}); core.AsError(err).Code != core.CodeQueueFull {
		t.Errorf("a second isolation over the limit: %v", err)
	}
	// Kept ones are counted apart: a shelf of them does not stop today's.
	kept, err := f.svc.Create(ctx, core.IsolationRequest{Audio: upload{wavBytes(10)}, Keep: true})
	if err != nil {
		t.Fatalf("a kept isolation beside one that is not: %v", err)
	}
	if _, err := f.svc.Create(ctx, core.IsolationRequest{Audio: upload{wavBytes(10)}, Keep: true}); core.AsError(err).Code != core.CodeQueueFull {
		t.Errorf("a second kept isolation over the limit: %v", err)
	}
	f.wait(t, ctx, kept.ID)
	keep := false
	if _, err := f.svc.Update(ctx, kept.ID, core.IsolationUpdate{Keep: &keep}); core.AsError(err).Code != core.CodeQueueFull {
		t.Errorf("releasing a kept one into a full set: %v", err)
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
