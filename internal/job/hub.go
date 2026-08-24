package job

import (
	"context"
	"sync"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Event is one job state transition.
//
// Seq is per job and starts at 1, which is what lets a reconnecting SSE client
// say what it already saw through Last-Event-ID.
type Event struct {
	Seq int64
	Job core.Job
}

// Hub fans job transitions out to whoever is watching.
//
// A topic lives only while its job is active. Once the job is terminal the final
// event goes out, subscribers are closed and the topic is dropped: a client that
// reconnects afterwards is served the finished job from the database instead,
// which is both simpler and more truthful than keeping a replay buffer alive for
// work that will never change again.
type Hub struct {
	mu     sync.Mutex
	topics map[string]*topic
	ring   int
}

type topic struct {
	seq  int64
	ring []Event
	last core.Job
	subs map[chan Event]struct{}
}

// NewHub keeps ring events per job for replay. Job transitions are few — queued,
// running, a handful of stages, terminal — so a small ring covers any realistic
// reconnect.
func NewHub(ring int) *Hub {
	if ring < 1 {
		ring = 16
	}
	return &Hub{topics: map[string]*topic{}, ring: ring}
}

// Publish records a transition and delivers it. A terminal status closes the
// topic, so Publish is also how a job stops being watchable.
func (h *Hub) Publish(j core.Job) {
	h.mu.Lock()
	defer h.mu.Unlock()

	t, ok := h.topics[j.ID]
	if !ok {
		if Terminal(j.Status) {
			// Nobody is watching and the job is over; a topic would only be
			// created to be destroyed.
			return
		}
		t = &topic{subs: map[chan Event]struct{}{}}
		h.topics[j.ID] = t
	}

	t.seq++
	t.last = j
	ev := Event{Seq: t.seq, Job: j}

	t.ring = append(t.ring, ev)
	if len(t.ring) > h.ring {
		t.ring = t.ring[len(t.ring)-h.ring:]
	}

	for ch := range t.subs {
		select {
		case ch <- ev:
		default:
			// A subscriber that cannot keep up with a job's own transitions is
			// gone, not slow. Dropping it beats blocking the worker that is
			// trying to report progress.
			delete(t.subs, ch)
			close(ch)
		}
	}

	if Terminal(j.Status) {
		for ch := range t.subs {
			close(ch)
		}
		delete(h.topics, j.ID)
	}
}

// Stage updates the running job's progress without changing its status. The
// pipeline reports stages through core.Observer; this is where they become
// something a client can see.
func (h *Hub) Stage(jobID, stage string, percent int) {
	h.mu.Lock()
	t, ok := h.topics[jobID]
	if !ok || t.last.Status != core.JobRunning {
		h.mu.Unlock()
		return
	}
	j := t.last
	h.mu.Unlock()

	j.Stage = stage
	j.Percent = percent
	h.Publish(j)
}

// Subscribe returns a channel of transitions with Seq greater than after,
// starting with whatever the replay buffer still holds, plus a cancel function.
//
// The bool reports whether the job is being tracked at all. False means it is
// either finished or unknown, and the caller should answer from the database.
func (h *Hub) Subscribe(jobID string, after int64) (<-chan Event, func(), bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	t, ok := h.topics[jobID]
	if !ok {
		return nil, func() {}, false
	}

	// Room for the replay plus the transitions still to come. Sized rather than
	// unbounded so a stalled reader is dropped instead of accumulating.
	ch := make(chan Event, h.ring+8)
	for _, ev := range t.ring {
		if ev.Seq <= after {
			continue
		}
		select {
		case ch <- ev:
		default:
		}
	}
	t.subs[ch] = struct{}{}

	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if cur, ok := h.topics[jobID]; ok {
			if _, still := cur.subs[ch]; still {
				delete(cur.subs, ch)
				close(ch)
			}
		}
	}
	return ch, cancel, true
}

// Watching reports how many jobs currently have a topic. Tests use it to prove
// topics are dropped rather than accumulated.
func (h *Hub) Watching() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.topics)
}

// stagePercent turns a pipeline stage into a coarse progress figure.
//
// These are fixed weights, not measurements: decoding a ten-minute file is fast
// and recognising it is not, so an evenly-spaced bar would sit at 75% for most
// of the wait. A client that needs real timing has stats in the result.
var stagePercent = map[string]int{
	"decode":   10,
	"vad":      25,
	"asr":      40,
	"assemble": 95,
}

// Progress adapts core.Observer onto the hub, so pipeline stages become job
// events without the pipeline knowing that jobs exist.
type Progress struct {
	hub *Hub
	obs core.Observer
}

func NewProgress(hub *Hub, next core.Observer) *Progress {
	if next == nil {
		next = core.NopObserver{}
	}
	return &Progress{hub: hub, obs: next}
}

func (p *Progress) StageStarted(ctx context.Context, jobID, stage string) {
	p.hub.Stage(jobID, stage, stagePercent[stage])
	p.obs.StageStarted(ctx, jobID, stage)
}

func (p *Progress) StageFinished(ctx context.Context, jobID, stage string, ms int64, err error) {
	p.obs.StageFinished(ctx, jobID, stage, ms, err)
}

func (p *Progress) QueueDepth(n int) { p.obs.QueueDepth(n) }

func (p *Progress) ModelEvent(id string, from, to core.ModelState) {
	p.obs.ModelEvent(id, from, to)
}

var _ core.Observer = (*Progress)(nil)
