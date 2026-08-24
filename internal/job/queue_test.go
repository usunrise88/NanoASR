package job

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/spool"
)

// memStore is an in-memory job.Store. The SQLite implementation has its own
// tests; the queue's behaviour should not depend on which store is underneath.
type memStore struct {
	mu      sync.Mutex
	records map[string]Record
	order   []string
	failGet bool
}

func newMemStore() *memStore { return &memStore{records: map[string]Record{}} }

func (m *memStore) Create(_ context.Context, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records[r.Job.ID] = r
	m.order = append(m.order, r.Job.ID)
	return nil
}

func (m *memStore) Update(_ context.Context, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.records[r.Job.ID]; !ok {
		return core.Errorf(core.CodeJobNotFound, "no such job")
	}
	m.records[r.Job.ID] = r
	return nil
}

func (m *memStore) Get(_ context.Context, id string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failGet {
		return Record{}, errors.New("store is unavailable")
	}
	r, ok := m.records[id]
	if !ok {
		return Record{}, core.Errorf(core.CodeJobNotFound, "no such job")
	}
	return r, nil
}

func (m *memStore) List(context.Context, core.JobFilter) ([]core.Job, string, error) {
	return nil, "", nil
}

func (m *memStore) Pending(context.Context) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Record
	for _, id := range m.order {
		if r := m.records[id]; r.Job.Status == core.JobQueued {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memStore) FailStale(context.Context, string) (int, error)    { return 0, nil }
func (m *memStore) Purge(context.Context, time.Duration) (int, error) { return 0, nil }

func (m *memStore) status(id string) core.JobStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.records[id].Job.Status
}

func (m *memStore) record(id string) Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.records[id]
}

// runnerFunc adapts a function to Runner.
type runnerFunc func(ctx context.Context, id string, req core.Request) (*core.Result, error)

func (f runnerFunc) Run(ctx context.Context, id string, req core.Request) (*core.Result, error) {
	return f(ctx, id, req)
}

// stubSource is audio that never touched a multipart form.
type stubSource struct {
	name string
	body []byte
}

func (s *stubSource) Open() (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(string(s.body))), nil
}
func (s *stubSource) Filename() string { return s.name }
func (s *stubSource) Size() int64      { return int64(len(s.body)) }
func (s *stubSource) Close() error     { return nil }

func request(size int) core.Request {
	return core.Request{
		Audio:    &stubSource{name: "call.wav", body: make([]byte, size)},
		ModelID:  "gigaam-v2-ctc-ru",
		Source:   core.SourceAPI,
		APIKeyID: "key-a",
	}
}

type fixture struct {
	q     *Queue
	store *memStore
	sp    *spool.Spool
	hub   *Hub
	dir   string
}

func newFixture(t *testing.T, runner Runner, opts ...func(*Options)) *fixture {
	t.Helper()

	dir := t.TempDir()
	sp := spool.New(dir, 0)
	hub := NewHub(16)
	store := newMemStore()

	opt := Options{Size: 8, Workers: 2, Spool: sp, Hub: hub}
	for _, o := range opts {
		o(&opt)
	}
	q := New(store, runner, opt)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = q.Shutdown(ctx)
	})
	return &fixture{q: q, store: store, sp: sp, hub: hub, dir: dir}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSubmitTakesOwnershipOfTheAudio(t *testing.T) {
	release := make(chan struct{})
	f := newFixture(t, runnerFunc(func(ctx context.Context, _ string, _ core.Request) (*core.Result, error) {
		<-release
		return &core.Result{Text: "ok"}, nil
	}))
	f.q.Start()

	job, err := f.q.Submit(context.Background(), request(1024), PriorityBatch)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if job.Status != core.JobQueued {
		t.Errorf("status = %q, want queued", job.Status)
	}

	// The whole point of the queue: the audio outlives the request that
	// carried it, and is accounted for while it waits.
	path := f.sp.Path(job.ID)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the accepted audio is not on disk: %v", err)
	}
	if f.sp.Used() != 1024 {
		t.Errorf("used = %d, want 1024", f.sp.Used())
	}

	close(release)
	waitFor(t, "the job to finish", func() bool { return f.store.status(job.ID) == core.JobSucceeded })

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the audio outlived the job: %v", err)
	}
	if f.sp.Used() != 0 {
		t.Errorf("used = %d after completion, want 0", f.sp.Used())
	}
}

func TestSubmitRefusesWhenTheQueueIsFull(t *testing.T) {
	release := make(chan struct{})
	f := newFixture(t, runnerFunc(func(context.Context, string, core.Request) (*core.Result, error) {
		<-release
		return &core.Result{}, nil
	}), func(o *Options) { o.Size = 2; o.Workers = 0 })
	defer close(release)

	for range 2 {
		if _, err := f.q.Submit(context.Background(), request(8), PriorityBatch); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}
	_, err := f.q.Submit(context.Background(), request(8), PriorityBatch)
	if core.AsError(err).Code != core.CodeQueueFull {
		t.Fatalf("Submit = %v, want queue_full", err)
	}
}

func TestSubmitRefusesWhenTheDiskBudgetIsSpent(t *testing.T) {
	f := newFixture(t, runnerFunc(func(context.Context, string, core.Request) (*core.Result, error) {
		return &core.Result{}, nil
	}), func(o *Options) { o.Workers = 0 })
	f.q.spool = spool.New(f.dir, 1000)

	if _, err := f.q.Submit(context.Background(), request(800), PriorityBatch); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	_, err := f.q.Submit(context.Background(), request(800), PriorityBatch)
	if core.AsError(err).Code != core.CodeQueueFull {
		t.Fatalf("Submit = %v, want queue_full", err)
	}

	// A refused submission must leave nothing behind: no reservation, and no
	// half-written file that Sweep would then have to guess about.
	if f.q.spool.Used() != 800 {
		t.Errorf("used = %d, want 800", f.q.spool.Used())
	}
	entries, _ := os.ReadDir(f.dir)
	if len(entries) != 1 {
		t.Errorf("spool holds %d files, want 1", len(entries))
	}
}

func TestInteractiveWorkOvertakesABatchBacklog(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
	)
	gate := make(chan struct{})
	f := newFixture(t, runnerFunc(func(_ context.Context, id string, req core.Request) (*core.Result, error) {
		<-gate
		mu.Lock()
		order = append(order, req.Language)
		mu.Unlock()
		return &core.Result{}, nil
	}), func(o *Options) { o.Workers = 1 })

	for _, lang := range []string{"batch-1", "batch-2", "batch-3"} {
		req := request(8)
		req.Language = lang
		if _, err := f.q.Submit(context.Background(), req, PriorityBatch); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}
	urgent := request(8)
	urgent.Language = "interactive"
	if _, err := f.q.Submit(context.Background(), urgent, PriorityInteractive); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	f.q.Start()
	close(gate)

	waitFor(t, "all four jobs", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 4
	})
	mu.Lock()
	defer mu.Unlock()
	if order[0] != "interactive" {
		t.Errorf("ran %v; the interactive job did not overtake the backlog", order)
	}
}

func TestCancelRemovesAQueuedJobAndItsAudio(t *testing.T) {
	f := newFixture(t, runnerFunc(func(context.Context, string, core.Request) (*core.Result, error) {
		t.Error("a cancelled job was executed")
		return nil, nil
	}), func(o *Options) { o.Workers = 0 })

	job, err := f.q.Submit(context.Background(), request(64), PriorityBatch)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := f.q.Cancel(context.Background(), job.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	if got := f.store.status(job.ID); got != core.JobCanceled {
		t.Errorf("status = %q, want canceled", got)
	}
	if f.q.Depth() != 0 {
		t.Errorf("depth = %d, want 0", f.q.Depth())
	}
	if _, err := os.Stat(f.sp.Path(job.ID)); !os.IsNotExist(err) {
		t.Error("cancelling left the audio behind")
	}
	if f.sp.Used() != 0 {
		t.Errorf("used = %d, want 0", f.sp.Used())
	}
}

func TestCancelStopsARunningJob(t *testing.T) {
	started := make(chan struct{})
	f := newFixture(t, runnerFunc(func(ctx context.Context, string2 string, _ core.Request) (*core.Result, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	f.q.Start()

	job, err := f.q.Submit(context.Background(), request(64), PriorityBatch)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	if err := f.q.Cancel(context.Background(), job.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitFor(t, "the cancelled job to settle", func() bool {
		return f.store.status(job.ID) == core.JobCanceled
	})
	// The worker's own finish must not overwrite the cancellation with a
	// failure derived from the context error.
	time.Sleep(50 * time.Millisecond)
	if got := f.store.status(job.ID); got != core.JobCanceled {
		t.Errorf("status settled at %q, want canceled", got)
	}
}

// Cancel resolves the job through the store rather than only through memory, so
// an id that exists nowhere is reported as such instead of answered "done".
// Callers reach this through pipeline.Cancel, which has already checked
// existence and ownership, so in practice this is a purge racing a request.
func TestCancelOfAnUnknownJobSaysSo(t *testing.T) {
	f := newFixture(t, runnerFunc(func(context.Context, string, core.Request) (*core.Result, error) {
		return &core.Result{}, nil
	}), func(o *Options) { o.Workers = 0 })

	err := f.q.Cancel(context.Background(), "job_missing")
	if core.AsError(err).Code != core.CodeJobNotFound {
		t.Fatalf("Cancel = %v, want job_not_found", err)
	}
}

// A finished job is not cancellable, and saying so is not an error: the client
// asked for it to stop, and it has.
func TestCancelOfAFinishedJobIsANoOp(t *testing.T) {
	f := newFixture(t, runnerFunc(func(context.Context, string, core.Request) (*core.Result, error) {
		return &core.Result{}, nil
	}), func(o *Options) { o.Workers = 0 })

	rec := NewRecord("job_done", request(8), PriorityBatch, time.Now())
	rec.Job.Status = core.JobSucceeded
	if err := f.store.Create(context.Background(), rec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.q.Cancel(context.Background(), "job_done"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got := f.store.status("job_done"); got != core.JobSucceeded {
		t.Errorf("status = %q; cancelling rewrote a finished job", got)
	}
}

func TestARunThatOverrunsIsATimeoutNotAFailure(t *testing.T) {
	f := newFixture(t, runnerFunc(func(ctx context.Context, string2 string, _ core.Request) (*core.Result, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}), func(o *Options) { o.MaxRunTime = 30 * time.Millisecond })
	f.q.Start()

	job, err := f.q.Submit(context.Background(), request(8), PriorityBatch)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitFor(t, "the timeout", func() bool { return f.store.status(job.ID) == core.JobFailed })

	rec := f.store.record(job.ID)
	if rec.Job.Error == nil || rec.Job.Error.Code != core.CodeProcessingTimeout {
		t.Fatalf("error = %+v, want processing_timeout", rec.Job.Error)
	}
}

func TestAFailedRunKeepsItsDomainErrorCode(t *testing.T) {
	f := newFixture(t, runnerFunc(func(context.Context, string, core.Request) (*core.Result, error) {
		return nil, core.Errorf(core.CodeUnsupportedMediaType, "not audio")
	}))
	f.q.Start()

	job, err := f.q.Submit(context.Background(), request(8), PriorityBatch)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitFor(t, "the failure", func() bool { return f.store.status(job.ID) == core.JobFailed })

	rec := f.store.record(job.ID)
	if rec.Job.Error == nil || rec.Job.Error.Code != core.CodeUnsupportedMediaType {
		t.Fatalf("error = %+v", rec.Job.Error)
	}
	if _, err := os.Stat(f.sp.Path(job.ID)); !os.IsNotExist(err) {
		t.Error("a failed job left its audio behind")
	}
}

func TestRecoverResumesQueuedWorkAndSweepSpsaresIt(t *testing.T) {
	done := make(chan string, 4)
	f := newFixture(t, runnerFunc(func(_ context.Context, id string, _ core.Request) (*core.Result, error) {
		done <- id
		return &core.Result{Text: "resumed"}, nil
	}), func(o *Options) { o.Workers = 0 })

	job, err := f.q.Submit(context.Background(), request(128), PriorityBatch)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// Audio with no job behind it: a process killed between 202 and the
	// database write leaves exactly this.
	orphan := filepath.Join(f.dir, spool.Name("job_orphan"))
	if err := os.WriteFile(orphan, []byte("stale"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// A second process over the same directory and store.
	next := New(f.store, runnerFunc(func(_ context.Context, id string, _ core.Request) (*core.Result, error) {
		done <- id
		return &core.Result{Text: "resumed"}, nil
	}), Options{Size: 8, Workers: 1, Spool: spool.New(f.dir, 0), Hub: NewHub(16)})

	live, err := next.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(live) != 1 || live[job.ID] != 128 {
		t.Fatalf("live = %v, want one entry for %s", live, job.ID)
	}

	removed, err := next.spool.Sweep(live)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed %d files, want 1 (the orphan)", removed)
	}
	if _, err := os.Stat(f.sp.Path(job.ID)); err != nil {
		t.Fatalf("Sweep deleted the resumed job's audio: %v", err)
	}
	if next.spool.Used() != 128 {
		t.Errorf("used = %d after recovery, want 128", next.spool.Used())
	}

	next.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = next.Shutdown(ctx)
	}()

	select {
	case got := <-done:
		if got != job.ID {
			t.Errorf("ran %s, want %s", got, job.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the resumed job never ran")
	}
}

func TestRecoverFailsAJobWhoseAudioIsGone(t *testing.T) {
	f := newFixture(t, runnerFunc(func(context.Context, string, core.Request) (*core.Result, error) {
		t.Error("a job with no audio was executed")
		return nil, nil
	}), func(o *Options) { o.Workers = 0 })

	job, err := f.q.Submit(context.Background(), request(16), PriorityBatch)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := os.Remove(f.sp.Path(job.ID)); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	next := New(f.store, f.q.runner, Options{Size: 8, Spool: spool.New(f.dir, 0), Hub: NewHub(16)})
	live, err := next.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("live = %v, want empty", live)
	}
	if got := f.store.status(job.ID); got != core.JobFailed {
		t.Errorf("status = %q, want failed", got)
	}
}

func TestShutdownWaitsForInFlightWork(t *testing.T) {
	finished := make(chan struct{})
	started := make(chan struct{})
	f := newFixture(t, runnerFunc(func(context.Context, string, core.Request) (*core.Result, error) {
		close(started)
		time.Sleep(50 * time.Millisecond)
		close(finished)
		return &core.Result{}, nil
	}))
	f.q.Start()

	if _, err := f.q.Submit(context.Background(), request(8), PriorityBatch); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.q.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("Shutdown returned while a job was still running")
	}
}

func TestShutdownCutsOffWorkThatOverrunsTheGrace(t *testing.T) {
	started := make(chan struct{})
	f := newFixture(t, runnerFunc(func(ctx context.Context, string2 string, _ core.Request) (*core.Result, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	f.q.Start()

	job, err := f.q.Submit(context.Background(), request(8), PriorityBatch)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := f.q.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want the grace period to expire", err)
	}
	// Cut short by the server, not by its owner.
	waitFor(t, "the interrupted job", func() bool { return f.store.status(job.ID) == core.JobFailed })
	if code := f.store.record(job.ID).Job.Error.Code; code != core.CodeDraining {
		t.Errorf("error code = %q, want draining", code)
	}
}

func TestSubmitAfterShutdownIsRefused(t *testing.T) {
	f := newFixture(t, runnerFunc(func(context.Context, string, core.Request) (*core.Result, error) {
		return &core.Result{}, nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := f.q.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	_, err := f.q.Submit(context.Background(), request(8), PriorityBatch)
	if core.AsError(err).Code != core.CodeDraining {
		t.Fatalf("Submit = %v, want draining", err)
	}
}

func TestPositionReflectsPriorityOrder(t *testing.T) {
	f := newFixture(t, runnerFunc(func(context.Context, string, core.Request) (*core.Result, error) {
		return &core.Result{}, nil
	}), func(o *Options) { o.Workers = 0 })

	first, _ := f.q.Submit(context.Background(), request(8), PriorityBatch)
	second, _ := f.q.Submit(context.Background(), request(8), PriorityBatch)
	urgent, _ := f.q.Submit(context.Background(), request(8), PriorityInteractive)

	if got := f.q.Position(urgent.ID); got != 1 {
		t.Errorf("interactive position = %d, want 1", got)
	}
	if got := f.q.Position(first.ID); got != 2 {
		t.Errorf("first batch position = %d, want 2", got)
	}
	if got := f.q.Position(second.ID); got != 3 {
		t.Errorf("second batch position = %d, want 3", got)
	}
	if got := f.q.Position("job_missing"); got != 0 {
		t.Errorf("unknown position = %d, want 0", got)
	}
}

// --- regressions ------------------------------------------------------------

// Submit checked the queue's length under the mutex and appended after
// releasing it, so concurrent submissions could both pass a check that only one
// of them should have. Overshooting by one was not itself the damage: the
// wake-up channel held exactly Size tokens, so the surplus token was dropped and
// the job it belonged to waited for a wake-up that only somebody else's
// submission could deliver. With no further traffic it waited forever, holding
// its audio and its share of the byte budget.
func TestConcurrentSubmitsNeitherOvershootNorStrand(t *testing.T) {
	const size = 4

	release := make(chan struct{})
	ran := make(chan string, 32)
	f := newFixture(t, runnerFunc(func(_ context.Context, id string, _ core.Request) (*core.Result, error) {
		<-release
		ran <- id
		return &core.Result{}, nil
	}), func(o *Options) { o.Size = size; o.Workers = 1 })

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted []string
		refused  int
	)
	for range size * 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := f.q.Submit(context.Background(), request(8), PriorityBatch)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted = append(accepted, j.ID)
			case core.AsError(err).Code == core.CodeQueueFull:
				refused++
			default:
				t.Errorf("Submit: %v", err)
			}
		}()
	}
	wg.Wait()

	if len(accepted) > size {
		t.Errorf("accepted %d jobs into a queue of %d", len(accepted), size)
	}
	if refused == 0 {
		t.Fatal("nothing was refused; the test did not create contention")
	}

	// Every accepted job must actually run. This is the part that used to fail:
	// the surplus job stayed in the queue with no token to wake a worker for it.
	f.q.Start()
	close(release)

	seen := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for len(seen) < len(accepted) {
		select {
		case id := <-ran:
			seen[id] = true
		case <-deadline:
			t.Fatalf("only %d of %d accepted jobs ran; the rest were stranded",
				len(seen), len(accepted))
		}
	}
}

// Recover stopped at the queue's capacity and left the remaining queued jobs out
// of the live set it returns. Sweep then read that set as the whole truth and
// deleted their audio, while the database still called them queued — so the next
// start failed them for audio that the previous start had removed. Owning the
// audio is not conditional on fitting in memory right now.
func TestRecoverKeepsTheAudioOfEveryQueuedJob(t *testing.T) {
	const size = 2

	f := newFixture(t, runnerFunc(func(context.Context, string, core.Request) (*core.Result, error) {
		return &core.Result{}, nil
	}), func(o *Options) { o.Size = size; o.Workers = 0 })

	var ids []string
	for range size {
		j, err := f.q.Submit(context.Background(), request(16), PriorityBatch)
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		ids = append(ids, j.ID)
	}
	// A third queued job, as a previous process with a larger queue_size would
	// have left behind.
	extra := NewRecord("job_extra", request(16), PriorityBatch, time.Now())
	if err := f.store.Create(context.Background(), extra); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(f.sp.Path("job_extra"), make([]byte, 16), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ids = append(ids, "job_extra")

	next := New(f.store, f.q.runner, Options{
		Size: size, Spool: spool.New(f.dir, 0), Hub: NewHub(16),
	})
	live, err := next.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if _, err := next.spool.Sweep(live); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	for _, id := range ids {
		if _, err := os.Stat(next.spool.Path(id)); err != nil {
			t.Errorf("the audio of queued job %s was swept away: %v", id, err)
		}
		if f.store.status(id) != core.JobQueued {
			t.Errorf("job %s became %q during recovery", id, f.store.status(id))
		}
	}
}

// Cancel only looked in memory. A job the queue does not hold — recovered by a
// process that has since restarted, or simply never enqueued — answered nil,
// which the handler reported as a successful cancellation of a job that stayed
// queued. Cancelling must not depend on what the queue happens to remember.
func TestCancelWritesTheTerminalStateForAJobTheQueueDoesNotHold(t *testing.T) {
	f := newFixture(t, runnerFunc(func(context.Context, string, core.Request) (*core.Result, error) {
		return &core.Result{}, nil
	}), func(o *Options) { o.Workers = 0 })

	rec := NewRecord("job_orphaned", request(32), PriorityBatch, time.Now())
	if err := f.store.Create(context.Background(), rec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(f.sp.Path("job_orphaned"), make([]byte, 32), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := f.q.Cancel(context.Background(), "job_orphaned"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got := f.store.status("job_orphaned"); got != core.JobCanceled {
		t.Errorf("status = %q, want canceled", got)
	}
	if _, err := os.Stat(f.sp.Path("job_orphaned")); !os.IsNotExist(err) {
		t.Error("cancelling left the audio behind")
	}
}
