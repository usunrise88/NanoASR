//go:build integration

// Integration tests for the asynchronous surface added in M3.
//
// Unit tests cover each layer against a double; these cover the three real ones
// together — the SQLite store, the queue and the pipeline over actual weights —
// because the parts that break here are the seams between them: audio ownership,
// restart ordering, and who is allowed to see what.
//
//	go test -tags integration ./internal/pipeline/... -run Jobs
package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/job"
	"github.com/usunrise88/nanoasr/internal/spool"
	"github.com/usunrise88/nanoasr/internal/store/sqlite"
)

// asyncStack is a pipeline with a real store and queue behind it.
type asyncStack struct {
	pipeline *Pipeline
	queue    *job.Queue
	store    *sqlite.Store
	spool    *spool.Spool
	dir      string
}

func newAsyncStack(t *testing.T, workers int) *asyncStack {
	t.Helper()
	return attachAsync(t, newStack(t), t.TempDir(), workers)
}

// attachAsync wires a queue onto a pipeline over dir, so a test can stand a
// second process up over the same state.
func attachAsync(t *testing.T, p *Pipeline, dir string, workers int) *asyncStack {
	t.Helper()

	store, err := sqlite.Open(filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sp := spool.New(filepath.Join(dir, "spool"), 0)
	q := job.New(store, p, job.Options{
		Size:       16,
		Workers:    workers,
		MaxRunTime: 5 * time.Minute,
		Spool:      sp,
		Hub:        job.NewHub(32),
	})
	p.Attach(q, store)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = q.Shutdown(ctx)
	})

	return &asyncStack{pipeline: p, queue: q, store: store, spool: sp, dir: dir}
}

// as builds a context carrying an API key's identity.
func as(keyID string) context.Context {
	return core.WithCaller(context.Background(), core.Caller{KeyID: keyID})
}

func submit(t *testing.T, s *asyncStack, ctx context.Context, name string) *core.Job {
	t.Helper()
	j, err := s.pipeline.Submit(ctx, core.Request{
		Audio:    fileSource{path: audioPath(t, name)},
		ModelID:  testModel,
		Source:   core.SourceAPI,
		APIKeyID: core.CallerOf(ctx).KeyID,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return j
}

func awaitTerminal(t *testing.T, s *asyncStack, ctx context.Context, id string) *core.Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		j, err := s.pipeline.Job(ctx, id)
		if err != nil {
			t.Fatalf("Job: %v", err)
		}
		if job.Terminal(j.Status) {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s never finished", id)
	return nil
}

func TestJobsRunEndToEndAndReleaseTheirAudio(t *testing.T) {
	s := newAsyncStack(t, 1)
	s.queue.Start()
	ctx := as("key-a")

	j := submit(t, s, ctx, "ru-16k.wav")
	if j.Status != core.JobQueued {
		t.Fatalf("status = %q, want queued", j.Status)
	}
	// The upload has outlived its request; that is the whole point.
	if _, err := os.Stat(s.spool.Path(j.ID)); err != nil {
		t.Fatalf("the accepted audio is not on disk: %v", err)
	}

	done := awaitTerminal(t, s, ctx, j.ID)
	if done.Status != core.JobSucceeded {
		t.Fatalf("status = %q, error = %+v", done.Status, done.Error)
	}
	if done.Result == nil || done.Result.Text == "" {
		t.Fatal("a succeeded job carries no transcript")
	}
	words := done.Result.Words()
	if len(words) == 0 {
		t.Fatal("no word timings survived the round trip through the database")
	}
	assertWordInvariants(t, words, done.Result.Duration)

	if _, err := os.Stat(s.spool.Path(j.ID)); !os.IsNotExist(err) {
		t.Errorf("the audio outlived the job: %v", err)
	}
	if s.spool.Used() != 0 {
		t.Errorf("used = %d after completion, want 0", s.spool.Used())
	}
}

func TestJobsWatchReportsEveryTransition(t *testing.T) {
	s := newAsyncStack(t, 1)
	s.queue.Start()
	ctx := as("key-a")

	j := submit(t, s, ctx, "ru-16k.wav")
	events, err := s.pipeline.Watch(ctx, j.ID, 0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	var seen []core.JobStatus
	var lastSeq int64
	stages := map[string]bool{}
	for ev := range events {
		if ev.Seq <= lastSeq {
			t.Errorf("sequence went backwards: %d after %d", ev.Seq, lastSeq)
		}
		lastSeq = ev.Seq
		if len(seen) == 0 || seen[len(seen)-1] != ev.Job.Status {
			seen = append(seen, ev.Job.Status)
		}
		if ev.Job.Stage != "" {
			stages[ev.Job.Stage] = true
		}
	}

	if len(seen) < 2 || seen[len(seen)-1] != core.JobSucceeded {
		t.Fatalf("statuses = %v, want them to end at succeeded", seen)
	}
	// Progress is the reason the UI can show something other than a spinner.
	for _, want := range []string{"decode", "vad", "asr"} {
		if !stages[want] {
			t.Errorf("stage %q was never reported; saw %v", want, stages)
		}
	}
}

// Reconnecting after the job is over must still answer, from the database.
func TestJobsWatchAfterCompletionYieldsACatchUp(t *testing.T) {
	s := newAsyncStack(t, 1)
	s.queue.Start()
	ctx := as("key-a")

	j := submit(t, s, ctx, "ru-16k.wav")
	awaitTerminal(t, s, ctx, j.ID)

	events, err := s.pipeline.Watch(ctx, j.ID, 99)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	var got []core.JobEvent
	for ev := range events {
		got = append(got, ev)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want a single catch-up", len(got))
	}
	if got[0].Job.Status != core.JobSucceeded {
		t.Errorf("catch-up = %+v", got[0].Job)
	}
	// Not a transition, so no sequence: an unchanging job must not get a new
	// event id on every reconnect.
	if got[0].Seq != 0 {
		t.Errorf("catch-up carried seq %d, want 0", got[0].Seq)
	}
}

func TestJobsAreInvisibleToAnotherKey(t *testing.T) {
	s := newAsyncStack(t, 1)
	s.queue.Start()

	mine, theirs := as("key-a"), as("key-b")
	j := submit(t, s, mine, "ru-16k.wav")
	awaitTerminal(t, s, mine, j.ID)

	// job_not_found rather than forbidden: a 403 would confirm the job exists.
	for name, call := range map[string]func() error{
		"Job":    func() error { _, err := s.pipeline.Job(theirs, j.ID); return err },
		"Cancel": func() error { return s.pipeline.Cancel(theirs, j.ID) },
		"Watch":  func() error { _, err := s.pipeline.Watch(theirs, j.ID, 0); return err },
	} {
		if code := core.AsError(call()).Code; code != core.CodeJobNotFound {
			t.Errorf("%s as another key = %q, want job_not_found", name, code)
		}
	}

	page, err := s.pipeline.ListJobs(theirs, core.JobFilter{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(page.Jobs) != 0 {
		t.Errorf("another key sees %d jobs", len(page.Jobs))
	}

	admin := core.WithCaller(context.Background(), core.Caller{KeyID: "key-b", Admin: true})
	page, err = s.pipeline.ListJobs(admin, core.JobFilter{})
	if err != nil {
		t.Fatalf("ListJobs as admin: %v", err)
	}
	if len(page.Jobs) != 1 {
		t.Errorf("admin sees %d jobs, want 1", len(page.Jobs))
	}
}

// The seam this milestone is really about: a queued job, its audio, and a
// process that goes away before the work starts.
func TestJobsQueuedWorkSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	first := attachAsync(t, newStack(t), dir, 0) // no workers: nothing drains
	ctx := as("key-a")

	j := submit(t, first, ctx, "ru-16k.wav")

	// An orphan from a process killed between the 202 and the database write.
	orphan := filepath.Join(first.spool.Dir(), spool.Name("job_orphan"))
	if err := os.WriteFile(orphan, []byte("stale"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := first.queue.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	_ = first.store.Close()

	// A second process over the same directory.
	second := attachAsync(t, newStack(t), dir, 1)
	live, err := second.queue.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if _, ok := live[j.ID]; !ok {
		t.Fatalf("the queued job was not recovered: %v", live)
	}

	removed, err := second.spool.Sweep(live)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed %d files, want 1 (the orphan)", removed)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("the orphan survived")
	}
	// Order is the whole risk here: sweeping before recovering deletes exactly
	// the audio the resumed job is waiting for.
	if _, err := os.Stat(second.spool.Path(j.ID)); err != nil {
		t.Fatalf("the resumed job's audio was deleted: %v", err)
	}

	second.queue.Start()
	done := awaitTerminal(t, second, ctx, j.ID)
	if done.Status != core.JobSucceeded {
		t.Fatalf("resumed job = %q, error = %+v", done.Status, done.Error)
	}
	if done.Result == nil || done.Result.Text == "" {
		t.Fatal("the resumed job produced no transcript")
	}
}

func TestJobsCancelStopsWorkAndFreesTheSpool(t *testing.T) {
	s := newAsyncStack(t, 0) // no workers: the job stays queued
	ctx := as("key-a")

	j := submit(t, s, ctx, "ru-16k.wav")
	if err := s.pipeline.Cancel(ctx, j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	got, err := s.pipeline.Job(ctx, j.ID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if got.Status != core.JobCanceled {
		t.Errorf("status = %q, want canceled", got.Status)
	}
	if _, err := os.Stat(s.spool.Path(j.ID)); !os.IsNotExist(err) {
		t.Error("cancelling left the audio behind")
	}
	if s.spool.Used() != 0 {
		t.Errorf("used = %d, want 0", s.spool.Used())
	}
}

func TestJobsHistoryPagesWithoutSkippingOrRepeating(t *testing.T) {
	s := newAsyncStack(t, 2)
	s.queue.Start()
	ctx := as("key-a")

	const total = 5
	for range total {
		submit(t, s, ctx, "ru-16k.wav")
	}

	seen := map[string]int{}
	cursor := ""
	for range total {
		page, err := s.pipeline.ListJobs(ctx, core.JobFilter{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("ListJobs: %v", err)
		}
		for _, j := range page.Jobs {
			seen[j.ID]++
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if len(seen) != total {
		t.Errorf("paged over %d jobs, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("job %s appeared %d times", id, n)
		}
	}
}
