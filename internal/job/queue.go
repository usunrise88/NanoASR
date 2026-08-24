// Package job runs transcription work asynchronously.
//
// The queue is bounded twice on purpose — by item count and by the bytes of
// audio it is holding — and answers 429 with Retry-After rather than accepting
// work it cannot start. An unbounded queue on a CPU-bound service converts a
// throughput problem into a timeout problem, and an unbounded spool converts it
// into a full disk.
//
// Admission is a reservation, not a check. Deciding "there is room" and then
// taking that room are separated by a file copy and a database write, so a bare
// check lets every caller in the window pass a test only one of them should
// have. Reserve first, convert the reservation into a queued item afterwards.
package job

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/spool"
)

// Priority lets an interactive request overtake a batch backlog.
type Priority int

const (
	PriorityBatch       Priority = 0
	PriorityInteractive Priority = 10
)

// Item is one unit of queued work.
type Item struct {
	ID       string
	Request  core.Request
	Priority Priority
	Enqueued time.Time

	cancel   context.CancelFunc
	canceled bool
}

// Store persists jobs and their results.
//
// Audio bytes are deliberately absent: the upload lives on disk under the job
// id (see internal/spool) and is removed the moment the job reaches a terminal
// state, so the database holds metadata and text only (SPEC §9).
type Store interface {
	Create(ctx context.Context, r Record) error
	Update(ctx context.Context, r Record) error
	Get(ctx context.Context, id string) (Record, error)
	// List returns one page plus the cursor for the next one, empty at the end.
	List(ctx context.Context, f core.JobFilter) ([]core.Job, string, error)
	// Pending returns the jobs left queued by a previous process, oldest first.
	// Their audio is still on disk, so they can be run rather than failed.
	Pending(ctx context.Context) ([]Record, error)
	// FailStale marks jobs left running by a crashed process as failed. Unlike
	// queued ones they cannot be resumed: how much of the work was already done
	// is unknowable.
	FailStale(ctx context.Context, reason string) (int, error)
	// Purge deletes history older than ttl.
	Purge(ctx context.Context, ttl time.Duration) (int, error)
}

// Runner executes one job. The pipeline provides it.
type Runner interface {
	Run(ctx context.Context, id string, req core.Request) (*core.Result, error)
}

// Notifier is told about a finished job. Webhook delivery implements it.
type Notifier interface {
	Notify(job core.Job, url string)
}

// Queue dispatches items to a fixed number of workers.
type Queue struct {
	mu   sync.Mutex
	cond *sync.Cond

	items   []*Item
	running map[string]*Item
	stopped bool
	// admitted counts accepted work that is not in items yet, so that the
	// window between accepting a job and queueing it cannot be used twice.
	admitted int

	store    Store
	runner   Runner
	obs      core.Observer
	hub      *Hub
	spool    *spool.Spool
	notifier Notifier
	log      *slog.Logger

	size       int
	workers    int
	maxRunTime time.Duration

	wg sync.WaitGroup
}

// Options configures the queue.
type Options struct {
	Size       int
	Workers    int
	MaxRunTime time.Duration
	Observer   core.Observer
	Hub        *Hub
	Spool      *spool.Spool
	Notifier   Notifier
	Logger     *slog.Logger
}

func New(store Store, runner Runner, opt Options) *Queue {
	if opt.Size < 1 {
		opt.Size = 1
	}
	if opt.Workers < 1 {
		opt.Workers = 1
	}
	if opt.Observer == nil {
		opt.Observer = core.NopObserver{}
	}
	if opt.Hub == nil {
		opt.Hub = NewHub(16)
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	q := &Queue{
		store:      store,
		runner:     runner,
		obs:        opt.Observer,
		hub:        opt.Hub,
		spool:      opt.Spool,
		notifier:   opt.Notifier,
		log:        opt.Logger,
		size:       opt.Size,
		workers:    opt.Workers,
		maxRunTime: opt.MaxRunTime,
		running:    map[string]*Item{},
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Depth is the number of queued items.
func (q *Queue) Depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Hub exposes the transition stream so a dialect can serve SSE.
func (q *Queue) Hub() *Hub { return q.hub }

// Submit accepts a request, takes ownership of its audio and enqueues it.
//
// Every failure here unwinds completely: an accepted job that has no audio, or
// audio nobody accounted for, is worse than a refusal.
func (q *Queue) Submit(ctx context.Context, req core.Request, priority Priority) (*core.Job, error) {
	id := NewID()

	if err := q.reserve(); err != nil {
		return nil, err
	}
	if q.spool == nil {
		q.unreserve()
		return nil, core.Errorf(core.CodeInternal, "the queue has no spool directory")
	}

	var size int64
	if req.Audio != nil {
		size = req.Audio.Size()
	}
	if err := q.spool.Reserve(id, size); err != nil {
		q.unreserve()
		return nil, err
	}

	audio, err := q.spool.Adopt(id, req.Audio)
	if err != nil {
		q.spool.Release(id)
		q.unreserve()
		return nil, err
	}
	req.Audio = audio

	rec := NewRecord(id, req, priority, time.Now())
	if err := q.store.Create(ctx, rec); err != nil {
		q.spool.Remove(id)
		q.unreserve()
		return nil, core.Errorf(core.CodeInternal, "cannot record the job").WithCause(err)
	}

	item := &Item{ID: id, Request: req, Priority: priority, Enqueued: rec.Job.CreatedAt}
	q.enqueue(item)

	job := rec.Job
	job.Position = q.Position(id)
	q.hub.Publish(job)
	return &job, nil
}

// reserve claims one of the queue's slots for work that is being accepted.
//
// Both the waiting items and the reservations count against the size: a caller
// whose audio is still being spooled has already been told yes.
func (q *Queue) reserve() error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.stopped {
		return core.Errorf(core.CodeDraining, "the server is shutting down")
	}
	if len(q.items)+q.admitted >= q.size {
		return core.Errorf(core.CodeQueueFull,
			"the queue is full (%d waiting); retry shortly", q.size)
	}
	q.admitted++
	return nil
}

// resume claims a slot for work a previous process already accepted.
//
// It cannot be refused, and that is the point: queue_size limits how much new
// work is taken on, not how much already-accepted work is carried forward. A
// job dropped here would have its audio swept away while the database still
// called it queued.
func (q *Queue) resume() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.admitted++
}

// unreserve gives a slot back when accepting the job failed.
func (q *Queue) unreserve() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.admitted > 0 {
		q.admitted--
	}
}

// enqueue turns a reservation into a queued item and wakes a worker.
func (q *Queue) enqueue(item *Item) {
	q.mu.Lock()
	if q.admitted > 0 {
		q.admitted--
	}
	q.items = append(q.items, item)
	q.sortLocked()
	depth := len(q.items)
	q.cond.Broadcast()
	q.mu.Unlock()

	q.obs.QueueDepth(depth)
}

// sortLocked keeps the highest priority first and, within a priority, the oldest
// first. A linear sort is right here: the queue is bounded at a few hundred
// items, and a heap would be a data structure chosen for its own sake.
func (q *Queue) sortLocked() {
	sort.SliceStable(q.items, func(i, j int) bool {
		if q.items[i].Priority != q.items[j].Priority {
			return q.items[i].Priority > q.items[j].Priority
		}
		return q.items[i].Enqueued.Before(q.items[j].Enqueued)
	})
}

// Position is a job's one-based place in the queue, or 0 if it is not waiting.
func (q *Queue) Position(id string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, it := range q.items {
		if it.ID == id {
			return i + 1
		}
	}
	return 0
}

// Start launches the workers.
func (q *Queue) Start() {
	for range q.workers {
		q.wg.Add(1)
		go q.work()
	}
}

func (q *Queue) work() {
	defer q.wg.Done()
	for {
		item, ok := q.next()
		if !ok {
			return
		}
		q.run(item)
	}
}

// next blocks until there is work or the queue is shutting down.
//
// Waiting on the condition rather than on a channel of wake-up tokens is what
// keeps the queue and the wake-ups from disagreeing: the items are the only
// state, so an item cannot exist with nothing to wake a worker for it.
func (q *Queue) next() (*Item, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for len(q.items) == 0 && !q.stopped {
		q.cond.Wait()
	}
	if q.stopped {
		// Whatever is still queued stays queued, in memory and in the
		// database; the next process resumes it.
		return nil, false
	}

	item := q.items[0]
	q.items = q.items[1:]
	q.running[item.ID] = item
	return item, true
}

// run executes one job and records the outcome exactly once.
func (q *Queue) run(item *Item) {
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if q.maxRunTime > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), q.maxRunTime)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	defer cancel()

	q.mu.Lock()
	item.cancel = cancel
	canceled := item.canceled
	q.mu.Unlock()
	if canceled {
		// Cancelled between pop and here; the terminal state is already written.
		return
	}

	started := time.Now()
	q.transition(ctx, item, func(r *Record) {
		r.Job.Status = core.JobRunning
		r.Job.StartedAt = &started
	})
	q.obs.QueueDepth(q.Depth())

	result, err := q.runner.Run(ctx, item.ID, item.Request)
	q.finish(item, result, err)
}

// finish writes the terminal state, releases the audio and notifies.
func (q *Queue) finish(item *Item, result *core.Result, runErr error) {
	q.mu.Lock()
	delete(q.running, item.ID)
	canceled := item.canceled
	q.mu.Unlock()

	if canceled {
		// Cancel already wrote the terminal state and removed the audio.
		return
	}

	finished := time.Now()
	// A background context: the request that started this is long gone, and the
	// outcome has to be recorded even when the job itself was cut short.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rec := q.transition(ctx, item, func(r *Record) {
		r.Job.FinishedAt = &finished
		switch {
		case runErr == nil:
			r.Job.Status = core.JobSucceeded
			r.Job.Result = result
			if result != nil {
				r.Job.ModelID = result.Model
				r.AudioSeconds = result.Duration
			}
		case errors.Is(runErr, context.DeadlineExceeded):
			r.Job.Status = core.JobFailed
			r.Job.Error = core.Errorf(core.CodeProcessingTimeout,
				"the job exceeded jobs.max_processing_time (%s)", q.maxRunTime)
		case errors.Is(runErr, context.Canceled) && q.draining():
			// Cut short by shutdown, not by a client. Calling that "canceled"
			// would tell the job's owner they did something they did not.
			r.Job.Status = core.JobFailed
			r.Job.Error = core.Errorf(core.CodeDraining,
				"the server shut down before this job finished")
		case errors.Is(runErr, context.Canceled):
			r.Job.Status = core.JobCanceled
		default:
			r.Job.Status = core.JobFailed
			r.Job.Error = core.AsError(runErr)
		}
	})

	q.spool.Remove(item.ID)
	q.obs.QueueDepth(q.Depth())

	if q.notifier != nil && item.Request.WebhookURL != "" {
		q.notifier.Notify(rec.Job, item.Request.WebhookURL)
	}
}

// transition applies a change to the stored record and publishes it. Reading the
// record back rather than mutating an in-memory copy is what keeps the database
// authoritative when two paths — a worker finishing and a client cancelling —
// touch the same job.
func (q *Queue) transition(ctx context.Context, item *Item, apply func(*Record)) Record {
	rec, err := q.store.Get(ctx, item.ID)
	if err != nil {
		q.log.Error("cannot read the job being updated", "job", item.ID, "err", err)
		rec = NewRecord(item.ID, item.Request, item.Priority, item.Enqueued)
	}
	apply(&rec)
	if err := q.store.Update(ctx, rec); err != nil {
		q.log.Error("cannot record a job transition",
			"job", item.ID, "status", rec.Job.Status, "err", err)
	}
	q.hub.Publish(rec.Job)
	return rec
}

// Cancel stops a job whether it is waiting or already running.
func (q *Queue) Cancel(ctx context.Context, id string) error {
	q.mu.Lock()
	var waiting *Item
	for i, it := range q.items {
		if it.ID == id {
			waiting = it
			q.items = append(q.items[:i], q.items[i+1:]...)
			break
		}
	}
	item := waiting
	if item == nil {
		item = q.running[id]
	}
	if item == nil {
		q.mu.Unlock()
		// Not in memory does not mean not cancellable: a job this process
		// never enqueued still has a row and a spooled file, and answering
		// "done" without touching either would leave it queued forever while
		// the client was told otherwise.
		return q.cancelStored(ctx, id)
	}
	item.canceled = true
	cancel := item.cancel
	q.mu.Unlock()

	if cancel != nil {
		// A running job stops between decode batches rather than instantly:
		// the context is checked around each batch, not inside the decoder.
		cancel()
	}

	// Removing the audio while the pipeline may still hold it open is safe:
	// an unlinked file stays readable through an open descriptor, and a job
	// being torn down has no reason to reopen it.

	finished := time.Now()
	q.transition(ctx, item, func(r *Record) {
		r.Job.Status = core.JobCanceled
		r.Job.FinishedAt = &finished
	})
	q.spool.Remove(id)
	q.obs.QueueDepth(q.Depth())
	return nil
}

// Recover puts the jobs left queued by a previous process back in line.
//
// It returns what is now live, which the caller passes to Spool.Sweep so that
// cleanup deletes orphans and nothing else. Doing it in the other order deletes
// exactly the audio these jobs are waiting for.
func (q *Queue) Recover(ctx context.Context) (map[string]int64, error) {
	pending, err := q.store.Pending(ctx)
	if err != nil {
		return nil, err
	}

	// Every queued job is carried forward, including any beyond queue_size:
	// that limit governs what new work is accepted, not what a previous process
	// already accepted. Dropping one here would leave its row queued while
	// Sweep, reading this map as the whole truth, deleted its audio.
	live := make(map[string]int64, len(pending))
	for _, rec := range pending {
		audio := q.spool.Source(rec.Job.ID, rec.Job.Filename, rec.AudioBytes)
		f, err := audio.Open()
		if err != nil {
			// The audio is gone, so the job cannot run. Say so instead of
			// leaving it queued forever.
			q.failRecovered(ctx, rec, "the audio of this job did not survive the restart")
			continue
		}
		_ = f.Close()

		live[rec.Job.ID] = rec.AudioBytes
		q.resume()
		q.enqueue(&Item{
			ID:       rec.Job.ID,
			Request:  rec.Params.Request(audio, rec.APIKeyID),
			Priority: rec.Priority,
			Enqueued: rec.Job.CreatedAt,
		})
	}
	if len(live) > 0 {
		q.log.Info("resumed queued jobs", "count", len(live))
	}
	if len(live) > q.size {
		q.log.Warn("more queued jobs were resumed than queue_size allows",
			"resumed", len(live), "queue_size", q.size,
			"note", "they will run, but no new work is accepted until the backlog drains")
	}
	return live, nil
}

func (q *Queue) failRecovered(ctx context.Context, rec Record, reason string) {
	finished := time.Now()
	rec.Job.Status = core.JobFailed
	rec.Job.FinishedAt = &finished
	rec.Job.Error = core.Errorf("audio_missing", "%s", reason)
	if err := q.store.Update(ctx, rec); err != nil {
		q.log.Error("cannot fail an unrecoverable job", "job", rec.Job.ID, "err", err)
	}
}

// Shutdown stops accepting work and waits for what is in flight.
//
// Jobs still queued stay queued in the database: their audio is on disk, and the
// next process resumes them. Only the running ones are cut short, and they are
// picked up by FailStale at the next start.
func (q *Queue) Shutdown(ctx context.Context) error {
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return nil
	}
	q.stopped = true
	q.cond.Broadcast()
	q.mu.Unlock()

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		q.cancelRunning()
		<-done
		return ctx.Err()
	}
}

// cancelStored cancels a job the queue does not hold, straight in the store.
func (q *Queue) cancelStored(ctx context.Context, id string) error {
	rec, err := q.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if Terminal(rec.Job.Status) {
		return nil // already over; cancelling it again changes nothing
	}

	finished := time.Now()
	rec.Job.Status = core.JobCanceled
	rec.Job.FinishedAt = &finished
	if err := q.store.Update(ctx, rec); err != nil {
		return core.Errorf(core.CodeInternal, "cannot cancel the job").WithCause(err)
	}
	q.hub.Publish(rec.Job)
	q.spool.Remove(id)
	return nil
}

// draining reports whether Shutdown has been called.
func (q *Queue) draining() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.stopped
}

func (q *Queue) cancelRunning() {
	q.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(q.running))
	for _, it := range q.running {
		if it.cancel != nil {
			cancels = append(cancels, it.cancel)
		}
	}
	q.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
}

// NewID is the job identifier: a prefix a human can recognise in a log plus
// enough randomness that ids cannot be walked.
func NewID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "job_" + hex.EncodeToString(b[:])
}
