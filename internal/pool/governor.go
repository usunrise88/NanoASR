// Package pool owns loaded models and the CPU they are allowed to use.
package pool

import (
	"context"
	"sync"
)

// Governor is a weighted semaphore over inference threads.
//
// It exists because onnxruntime allocates a thread pool per loaded model: three
// models at four threads each on a four-core box is twelve runnable threads
// fighting for four cores, and throughput drops instead of rising. Every batch
// acquires its thread budget here first (SPEC §12.2).
type Governor struct {
	mu       sync.Mutex
	cond     *sync.Cond
	capacity int
	used     int
}

// NewGovernor caps total concurrent inference threads.
func NewGovernor(capacity int) *Governor {
	if capacity < 1 {
		capacity = 1
	}
	g := &Governor{capacity: capacity}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// Acquire blocks until n slots are free or ctx is done.
//
// n larger than capacity is clamped rather than deadlocking: a model configured
// with more threads than the machine has should run slowly, not never.
func (g *Governor) Acquire(ctx context.Context, n int) error {
	if n < 1 {
		n = 1
	}
	if n > g.capacity {
		n = g.capacity
	}

	// Wake the waiter when ctx is cancelled; sync.Cond has no ctx-aware Wait.
	stop := context.AfterFunc(ctx, func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.cond.Broadcast()
	})
	defer stop()

	g.mu.Lock()
	defer g.mu.Unlock()
	for g.used+n > g.capacity {
		if err := ctx.Err(); err != nil {
			return err
		}
		g.cond.Wait()
	}
	g.used += n
	return nil
}

// Release returns n slots.
func (g *Governor) Release(n int) {
	if n < 1 {
		n = 1
	}
	if n > g.capacity {
		n = g.capacity
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.used -= n
	if g.used < 0 {
		g.used = 0
	}
	g.cond.Broadcast()
}

// Stats reports current usage, for /healthz and for future metrics.
func (g *Governor) Stats() (used, capacity int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.used, g.capacity
}
