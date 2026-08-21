// Package job runs transcription work asynchronously.
//
// The queue is bounded on purpose: when it is full the server answers 429 with
// Retry-After instead of accepting work it cannot start, because an unbounded
// queue on a CPU-bound service only converts a throughput problem into a
// timeout problem.
package job

import (
	"context"
	"sync"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
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

	cancel context.CancelFunc
}

// Store persists jobs and their results.
//
// Audio is deliberately absent from this interface: uploaded audio never
// outlives the active job (SPEC §2.1, §9), so there is nothing to persist.
type Store interface {
	Create(ctx context.Context, j core.Job) error
	Update(ctx context.Context, j core.Job) error
	Get(ctx context.Context, id string) (core.Job, error)
	List(ctx context.Context, f core.JobFilter) ([]core.Job, error)
	// FailStale marks jobs left running by a crashed process as failed. They
	// cannot be resumed because their audio is gone.
	FailStale(ctx context.Context, reason string) (int, error)
	// Purge deletes history older than ttl.
	Purge(ctx context.Context, ttl time.Duration) (int, error)
}

// Runner executes one job. The pipeline provides it.
type Runner interface {
	Run(ctx context.Context, id string, req core.Request) (*core.Result, error)
}

// Queue dispatches items to a fixed number of workers.
type Queue struct {
	mu      sync.Mutex
	items   []*Item
	waiting chan struct{}

	store  Store
	runner Runner
	obs    core.Observer

	size       int
	workers    int
	maxRunTime time.Duration

	wg   sync.WaitGroup
	stop chan struct{}
}

// Options configures the queue.
type Options struct {
	Size       int
	Workers    int
	MaxRunTime time.Duration
	Observer   core.Observer
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
	return &Queue{
		store:      store,
		runner:     runner,
		obs:        opt.Observer,
		size:       opt.Size,
		workers:    opt.Workers,
		maxRunTime: opt.MaxRunTime,
		waiting:    make(chan struct{}, 1),
		stop:       make(chan struct{}),
	}
}

// Depth is the number of queued items.
func (q *Queue) Depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// TODO(M3): Submit, worker loop with priority ordering, per-job context with
// MaxRunTime, cancellation via DELETE, SSE fan-out of state transitions, and
// Store persistence on every transition.
func (q *Queue) Submit(context.Context, *Item) error { return core.ErrNotImplemented }

// TODO(M3): start q.workers goroutines; each pops the highest-priority item,
// runs it and records the outcome.
func (q *Queue) Start() {}

// TODO(M3): stop accepting work, wait for in-flight jobs up to the shutdown
// grace period, then cancel the rest.
func (q *Queue) Shutdown(context.Context) error { return nil }
