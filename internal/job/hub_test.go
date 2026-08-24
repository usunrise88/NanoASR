package job

import (
	"context"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

func job(id string, s core.JobStatus) core.Job {
	return core.Job{ID: id, Status: s, ModelID: "gigaam-v2-ctc-ru"}
}

func receive(t *testing.T, ch <-chan Event) (Event, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for an event")
		return Event{}, false
	}
}

func TestSubscriberSeesTransitionsInOrder(t *testing.T) {
	h := NewHub(16)
	h.Publish(job("a", core.JobQueued))

	ch, cancel, ok := h.Subscribe("a", 0)
	if !ok {
		t.Fatal("Subscribe reported no topic for a queued job")
	}
	defer cancel()

	// The replayed transition comes first, so a client that connects a moment
	// late still learns how the job got here.
	ev, _ := receive(t, ch)
	if ev.Seq != 1 || ev.Job.Status != core.JobQueued {
		t.Fatalf("first event = %+v", ev)
	}

	h.Publish(job("a", core.JobRunning))
	ev, _ = receive(t, ch)
	if ev.Seq != 2 || ev.Job.Status != core.JobRunning {
		t.Fatalf("second event = %+v", ev)
	}
}

func TestLastEventIDSkipsWhatTheClientAlreadySaw(t *testing.T) {
	h := NewHub(16)
	h.Publish(job("a", core.JobQueued))
	h.Publish(job("a", core.JobRunning))

	ch, cancel, ok := h.Subscribe("a", 1)
	if !ok {
		t.Fatal("Subscribe reported no topic")
	}
	defer cancel()

	ev, _ := receive(t, ch)
	if ev.Seq != 2 {
		t.Fatalf("resumed at seq %d, want 2", ev.Seq)
	}
}

func TestATerminalStatusClosesTheStreamAndDropsTheTopic(t *testing.T) {
	h := NewHub(16)
	h.Publish(job("a", core.JobRunning))

	ch, cancel, _ := h.Subscribe("a", 0)
	defer cancel()
	receive(t, ch) // running

	h.Publish(job("a", core.JobSucceeded))

	ev, ok := receive(t, ch)
	if !ok || ev.Job.Status != core.JobSucceeded {
		t.Fatalf("final event = %+v, ok=%v", ev, ok)
	}
	if _, ok := receive(t, ch); ok {
		t.Fatal("the stream stayed open after a terminal status")
	}
	// The buffer is not kept alive for work that will never change again; a
	// late reconnect is answered from the database instead.
	if h.Watching() != 0 {
		t.Errorf("watching %d topics after completion, want 0", h.Watching())
	}
}

func TestSubscribeToAFinishedJobReportsNoTopic(t *testing.T) {
	h := NewHub(16)
	if _, _, ok := h.Subscribe("never-existed", 0); ok {
		t.Fatal("Subscribe invented a topic")
	}
}

func TestPublishingATerminalStatusNobodyWatchesCreatesNothing(t *testing.T) {
	h := NewHub(16)
	h.Publish(job("a", core.JobSucceeded))
	if h.Watching() != 0 {
		t.Fatalf("watching %d topics, want 0", h.Watching())
	}
}

func TestCancelUnsubscribes(t *testing.T) {
	h := NewHub(16)
	h.Publish(job("a", core.JobQueued))

	ch, cancel, _ := h.Subscribe("a", 0)
	receive(t, ch)
	cancel()

	if _, ok := <-ch; ok {
		t.Fatal("the channel stayed open after cancel")
	}
	cancel() // must be safe twice: an SSE handler defers it and may also call it
}

func TestStageUpdatesOnlyARunningJob(t *testing.T) {
	h := NewHub(16)
	h.Publish(job("a", core.JobQueued))

	ch, cancel, _ := h.Subscribe("a", 0)
	defer cancel()
	receive(t, ch)

	// Queued work has no stage; reporting one would be a lie about what the
	// server is doing.
	h.Stage("a", "decode", 10)
	select {
	case ev := <-ch:
		t.Fatalf("a queued job reported stage %q", ev.Job.Stage)
	case <-time.After(20 * time.Millisecond):
	}

	h.Publish(job("a", core.JobRunning))
	receive(t, ch)

	h.Stage("a", "asr", 40)
	ev, _ := receive(t, ch)
	if ev.Job.Stage != "asr" || ev.Job.Percent != 40 {
		t.Fatalf("stage event = %+v", ev.Job)
	}
	if ev.Job.Status != core.JobRunning {
		t.Errorf("a stage update changed the status to %q", ev.Job.Status)
	}
}

func TestStageOfAnUnknownJobIsIgnored(t *testing.T) {
	NewHub(16).Stage("nope", "decode", 10)
}

func TestASlowSubscriberIsDroppedRatherThanBlockingTheWorker(t *testing.T) {
	h := NewHub(2)
	h.Publish(job("a", core.JobQueued))

	ch, cancel, _ := h.Subscribe("a", 0)
	defer cancel()

	// Never read. The channel is ring+8 deep, so overflowing it takes a while;
	// what matters is that Publish returns instead of blocking.
	done := make(chan struct{})
	go func() {
		for range 200 {
			h.Publish(job("a", core.JobRunning))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a subscriber that stopped reading")
	}
	if _, ok := <-ch; ok {
		// Drain until closed; a dropped subscriber's channel is closed.
		for range ch {
		}
	}
}

func TestProgressTurnsPipelineStagesIntoJobEvents(t *testing.T) {
	h := NewHub(16)
	p := NewProgress(h, nil)

	h.Publish(job("a", core.JobRunning))
	ch, cancel, _ := h.Subscribe("a", 0)
	defer cancel()
	receive(t, ch)

	p.StageStarted(context.Background(), "a", "vad")
	ev, _ := receive(t, ch)
	if ev.Job.Stage != "vad" || ev.Job.Percent != stagePercent["vad"] {
		t.Fatalf("event = %+v", ev.Job)
	}
}

func TestProgressForwardsToTheNextObserver(t *testing.T) {
	var seen []string
	p := NewProgress(NewHub(16), recorder{&seen})

	ctx := context.Background()
	p.StageStarted(ctx, "a", "decode")
	p.StageFinished(ctx, "a", "decode", 5, nil)
	p.QueueDepth(3)
	p.ModelEvent("m", core.ModelAbsent, core.ModelReady)

	want := []string{"started:decode", "finished:decode", "depth:3", "model:m"}
	if len(seen) != len(want) {
		t.Fatalf("observer saw %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("observer[%d] = %q, want %q", i, seen[i], want[i])
		}
	}
}

type recorder struct{ into *[]string }

func (r recorder) StageStarted(_ context.Context, _, stage string) {
	*r.into = append(*r.into, "started:"+stage)
}
func (r recorder) StageFinished(_ context.Context, _, stage string, _ int64, _ error) {
	*r.into = append(*r.into, "finished:"+stage)
}
func (r recorder) QueueDepth(n int) {
	*r.into = append(*r.into, "depth:"+string(rune('0'+n)))
}
func (r recorder) ModelEvent(id string, _, _ core.ModelState) {
	*r.into = append(*r.into, "model:"+id)
}
