package sqlite

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/job"
)

func open(t *testing.T) *Store {
	t.Helper()
	// A nested directory on purpose: a fresh install points db_path somewhere
	// that does not exist yet.
	s, err := Open(filepath.Join(t.TempDir(), "state", "nanoasr.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func record(id string, created time.Time, opts ...func(*job.Record)) job.Record {
	r := job.Record{
		Job: core.Job{
			ID:        id,
			Status:    core.JobQueued,
			ModelID:   "gigaam-v2-ctc-ru",
			ModelRev:  "v2",
			Filename:  "call.wav",
			Source:    core.SourceAPI,
			CreatedAt: created,
		},
		Params:     job.Params{ModelID: "gigaam-v2-ctc-ru", Language: "ru", Punctuate: true},
		Priority:   job.PriorityBatch,
		APIKeyID:   "key-a",
		AudioBytes: 1024,
	}
	for _, o := range opts {
		o(&r)
	}
	return r
}

func owner(id string) func(*job.Record) {
	return func(r *job.Record) { r.APIKeyID = id }
}

func status(s core.JobStatus) func(*job.Record) {
	return func(r *job.Record) { r.Job.Status = s }
}

func TestRecordSurvivesTheRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	created := time.Now().Truncate(time.Millisecond)

	in := record("job_1", created)
	in.Params.Hotwords = []string{"нанoasr", "телефония"}
	if err := s.Create(ctx, in); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, "job_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Job.Status != core.JobQueued || got.Job.ModelID != in.Job.ModelID {
		t.Errorf("job = %+v", got.Job)
	}
	if !got.Job.CreatedAt.Equal(created) {
		t.Errorf("created_at = %v, want %v", got.Job.CreatedAt, created)
	}
	if got.APIKeyID != "key-a" || got.AudioBytes != 1024 {
		t.Errorf("owner/bytes = %q/%d", got.APIKeyID, got.AudioBytes)
	}
	// Params are what a restart needs; losing them silently would turn a
	// resumed job into a differently-configured one.
	if len(got.Params.Hotwords) != 2 || !got.Params.Punctuate || got.Params.Language != "ru" {
		t.Errorf("params = %+v", got.Params)
	}
}

func TestUpdateWritesTheOutcomeAndKeepsTheImmutableColumns(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	in := record("job_1", time.Now())
	if err := s.Create(ctx, in); err != nil {
		t.Fatalf("Create: %v", err)
	}

	started := time.Now().Truncate(time.Millisecond)
	finished := started.Add(2 * time.Second)
	done := in
	done.Job.Status = core.JobSucceeded
	done.Job.StartedAt = &started
	done.Job.FinishedAt = &finished
	done.Job.Result = &core.Result{ID: "job_1", Text: "привет", Duration: 1.5}
	done.AudioSeconds = 1.5
	// A caller with a stale copy must not be able to rewrite ownership.
	done.APIKeyID = "key-attacker"
	done.Params.Language = "en"

	if err := s.Update(ctx, done); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := s.Get(ctx, "job_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Job.Status != core.JobSucceeded {
		t.Errorf("status = %q", got.Job.Status)
	}
	if got.Job.Result == nil || got.Job.Result.Text != "привет" {
		t.Errorf("result = %+v", got.Job.Result)
	}
	if got.Job.StartedAt == nil || !got.Job.StartedAt.Equal(started) {
		t.Errorf("started_at = %v", got.Job.StartedAt)
	}
	if got.APIKeyID != "key-a" {
		t.Errorf("ownership was rewritten by Update: %q", got.APIKeyID)
	}
	if got.Params.Language != "ru" {
		t.Errorf("params were rewritten by Update: %+v", got.Params)
	}
}

func TestUpdateReportsAMissingJob(t *testing.T) {
	s := open(t)
	err := s.Update(context.Background(), record("nope", time.Now()))
	if core.AsError(err).Code != core.CodeJobNotFound {
		t.Fatalf("err = %v, want job_not_found", err)
	}
}

func TestGetReportsAMissingJob(t *testing.T) {
	s := open(t)
	_, err := s.Get(context.Background(), "nope")
	if core.AsError(err).Code != core.CodeJobNotFound {
		t.Fatalf("err = %v, want job_not_found", err)
	}
}

func TestErrorRoundTripsAsADomainError(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	in := record("job_1", time.Now())
	if err := s.Create(ctx, in); err != nil {
		t.Fatalf("Create: %v", err)
	}
	in.Job.Status = core.JobFailed
	in.Job.Error = core.Errorf(core.CodeProcessingTimeout, "exceeded 5m")
	if err := s.Update(ctx, in); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := s.Get(ctx, "job_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Job.Error == nil || got.Job.Error.Code != core.CodeProcessingTimeout {
		t.Fatalf("error = %+v", got.Job.Error)
	}
	if got.Job.Error.Message != "exceeded 5m" {
		t.Errorf("message = %q", got.Job.Error.Message)
	}
}

// The cursor must survive a whole page created inside one millisecond, which is
// exactly what a burst of submissions produces.
func TestCursorPagesThroughJobsSharingATimestamp(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	created := time.Now().Truncate(time.Millisecond)

	const total = 10
	for i := range total {
		id := string(rune('a'+i)) + "_job"
		if err := s.Create(ctx, record(id, created)); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	seen := map[string]int{}
	cursor := ""
	for range total { // bounded so a broken cursor cannot loop forever
		page, next, err := s.List(ctx, core.JobFilter{APIKeyID: "key-a", Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, j := range page {
			seen[j.ID]++
		}
		if next == "" {
			break
		}
		cursor = next
	}

	if len(seen) != total {
		t.Errorf("saw %d distinct jobs, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("job %s appeared %d times", id, n)
		}
	}
}

func TestListRejectsAMalformedCursor(t *testing.T) {
	s := open(t)
	_, _, err := s.List(context.Background(), core.JobFilter{Cursor: "not-base64!!"})
	if core.AsError(err).Code != core.CodeInvalidRequest {
		t.Fatalf("err = %v, want invalid_request", err)
	}
}

func TestListScopesHistoryToTheCallingKey(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()

	for _, r := range []job.Record{
		record("mine_1", now, owner("key-a")),
		record("mine_2", now.Add(time.Millisecond), owner("key-a")),
		record("theirs", now.Add(2*time.Millisecond), owner("key-b")),
	} {
		if err := s.Create(ctx, r); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	mine, _, err := s.List(ctx, core.JobFilter{APIKeyID: "key-a"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(mine) != 2 {
		t.Fatalf("got %d jobs, want 2: %+v", len(mine), mine)
	}
	for _, j := range mine {
		if j.ID == "theirs" {
			t.Fatal("another key's job is visible")
		}
	}

	all, _, err := s.List(ctx, core.JobFilter{APIKeyID: "key-a", Admin: true})
	if err != nil {
		t.Fatalf("List admin: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("admin saw %d jobs, want 3", len(all))
	}
}

// include_result=false is what the history listing uses: every row carries
// the full transcript by default and a page of long jobs is a JSON-round-trip
// the UI does not need to render a list.
func TestListSkipsTheResultColumnWhenAsked(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	r := record("job_1", time.Now(), status(core.JobSucceeded))
	r.Job.Result = &core.Result{Text: "full transcript the page does not need"}
	if err := s.Create(ctx, r); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Update(ctx, r); err != nil {
		t.Fatalf("Update: %v", err)
	}

	with, _, err := s.List(ctx, core.JobFilter{APIKeyID: "key-a"})
	if err != nil {
		t.Fatalf("List default: %v", err)
	}
	if len(with) != 1 || with[0].Result == nil || with[0].Result.Text != "full transcript the page does not need" {
		t.Fatalf("default listing returned %+v, want the result blob", with)
	}

	without, _, err := s.List(ctx, core.JobFilter{APIKeyID: "key-a", OmitResult: true})
	if err != nil {
		t.Fatalf("List no-result: %v", err)
	}
	if len(without) != 1 || without[0].Result != nil {
		t.Fatalf("no-result listing returned %+v, want Result nil", without)
	}
	if without[0].ID != "job_1" || without[0].Status != core.JobSucceeded {
		t.Errorf("the rest of the row is wrong: %+v", without[0])
	}
}

// In open mode nothing sets an api key, so unowned jobs must be visible to the
// unowned filter rather than to nobody.
func TestListWithoutAKeyMatchesUnownedJobs(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.Create(ctx, record("job_1", time.Now(), owner(""))); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, _, err := s.List(ctx, core.JobFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d jobs, want 1", len(got))
	}
}

func TestListFiltersByStatusModelAndTime(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)

	mustCreate(t, s, record("old", base, status(core.JobSucceeded)))
	mustCreate(t, s, record("new", base.Add(30*time.Minute), status(core.JobFailed)))
	mustCreate(t, s, record("other", base.Add(40*time.Minute), status(core.JobFailed),
		func(r *job.Record) { r.Job.ModelID = "zipformer-small-en" }))

	failed, _, err := s.List(ctx, core.JobFilter{
		APIKeyID: "key-a",
		Status:   []core.JobStatus{core.JobFailed},
		ModelID:  "gigaam-v2-ctc-ru",
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(failed) != 1 || failed[0].ID != "new" {
		t.Fatalf("got %+v, want just \"new\"", failed)
	}

	since := base.Add(35 * time.Minute)
	recent, _, err := s.List(ctx, core.JobFilter{APIKeyID: "key-a", Since: &since})
	if err != nil {
		t.Fatalf("List since: %v", err)
	}
	if len(recent) != 1 || recent[0].ID != "other" {
		t.Fatalf("got %+v, want just \"other\"", recent)
	}
}

func TestPendingReturnsQueuedWorkOldestFirst(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()

	mustCreate(t, s, record("second", now.Add(time.Second)))
	mustCreate(t, s, record("first", now))
	mustCreate(t, s, record("running", now, status(core.JobRunning)))
	mustCreate(t, s, record("done", now, status(core.JobSucceeded)))

	pending, err := s.Pending(ctx)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("got %d pending, want 2: %+v", len(pending), pending)
	}
	if pending[0].Job.ID != "first" || pending[1].Job.ID != "second" {
		t.Errorf("order = %s, %s", pending[0].Job.ID, pending[1].Job.ID)
	}
}

func TestFailStaleTouchesOnlyRunningJobs(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()

	mustCreate(t, s, record("queued", now))
	mustCreate(t, s, record("running", now, status(core.JobRunning)))
	mustCreate(t, s, record("done", now, status(core.JobSucceeded)))

	n, err := s.FailStale(ctx, "the server restarted while this job was running")
	if err != nil {
		t.Fatalf("FailStale: %v", err)
	}
	if n != 1 {
		t.Fatalf("failed %d jobs, want 1", n)
	}

	stale, err := s.Get(ctx, "running")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stale.Job.Status != core.JobFailed || stale.Job.Error == nil {
		t.Fatalf("stale job = %+v", stale.Job)
	}
	if stale.Job.Error.Code != "server_restart" {
		t.Errorf("error code = %q, want server_restart", stale.Job.Error.Code)
	}
	if stale.Job.FinishedAt == nil {
		t.Error("finished_at was not set")
	}

	// Queued work is resumable, so it must not be collateral damage.
	queued, err := s.Get(ctx, "queued")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if queued.Job.Status != core.JobQueued {
		t.Errorf("queued job became %q", queued.Job.Status)
	}
}

func TestPurgeDeletesOldHistoryAndNothingActive(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)

	finished := old
	mustCreate(t, s, record("old-done", old, status(core.JobSucceeded),
		func(r *job.Record) { r.Job.FinishedAt = &finished }))
	// No finished_at: a job that expired before it ever started still has to age
	// out, or it sits in history forever.
	mustCreate(t, s, record("old-expired", old, status(core.JobExpired)))
	mustCreate(t, s, record("old-running", old, status(core.JobRunning)))
	mustCreate(t, s, record("fresh-done", time.Now(), status(core.JobSucceeded)))

	n, err := s.Purge(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if n != 2 {
		t.Fatalf("purged %d, want 2", n)
	}
	for _, id := range []string{"old-running", "fresh-done"} {
		if _, err := s.Get(ctx, id); err != nil {
			t.Errorf("Purge removed %s: %v", id, err)
		}
	}
}

func TestPurgeWithoutATTLKeepsEverything(t *testing.T) {
	s := open(t)
	mustCreate(t, s, record("done", time.Now().Add(-time.Hour), status(core.JobSucceeded)))
	n, err := s.Purge(context.Background(), 0)
	if err != nil || n != 0 {
		t.Fatalf("Purge(0) = %d, %v", n, err)
	}
}

// WAL is the reason readers do not have to wait for the writer. If the pragma
// were applied to one pooled connection instead of all of them, this is where it
// would show up as "database is locked".
func TestHistoryReadsRunAlongsideAWriter(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 64)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 50 {
			r := record("w_"+string(rune('a'+i%26))+string(rune('a'+i/26)), time.Now())
			if err := s.Create(ctx, r); err != nil {
				errs <- err
				return
			}
			r.Job.Status = core.JobRunning
			if err := s.Update(ctx, r); err != nil {
				errs <- err
				return
			}
		}
	}()

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if _, _, err := s.List(ctx, core.JobFilter{APIKeyID: "key-a", Limit: 10}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent access: %v", err)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nanoasr.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := first.Create(context.Background(), record("job_1", time.Now())); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	if _, err := second.Get(context.Background(), "job_1"); err != nil {
		t.Errorf("job did not survive reopening: %v", err)
	}
}

func TestOpenRejectsAnEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") succeeded")
	}
}

func TestCursorDecodingRejectsGarbage(t *testing.T) {
	for name, in := range map[string]string{
		"not base64":    "not-base64!!",
		"no separator":  base64.RawURLEncoding.EncodeToString([]byte("foo")),
		"empty id":      base64.RawURLEncoding.EncodeToString([]byte("123:")),
		"unparsed time": base64.RawURLEncoding.EncodeToString([]byte("xx:yy")),
	} {
		if _, _, err := decodeCursor(in); core.AsError(err).Code != core.CodeInvalidRequest {
			t.Errorf("%s: decodeCursor(%q) = %v, want invalid_request", name, in, err)
		}
	}
}

func TestCursorRoundTrips(t *testing.T) {
	created := time.Now().Truncate(time.Millisecond)
	ms, id, err := decodeCursor(encodeCursor(core.Job{ID: "job_1", CreatedAt: created}))
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if ms != created.UnixMilli() || id != "job_1" {
		t.Errorf("got %d/%q, want %d/job_1", ms, id, created.UnixMilli())
	}
}

func mustCreate(t *testing.T, s *Store, r job.Record) {
	t.Helper()
	if err := s.Create(context.Background(), r); err != nil {
		t.Fatalf("Create %s: %v", r.Job.ID, err)
	}
}
