package realtime

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/words"
)

// eventBuffer is how many events a session holds for a client that is not
// reading fast enough. Partial hypotheses are dropped when it fills, because
// the next one supersedes them; anything else filling it means the client has
// stopped reading its own transcript, and the session is closed.
const eventBuffer = 64

// appendPoll is how often a full buffer is re-checked while Append waits for
// room. The decoder drains on its own tick, so this only has to be shorter
// than that to add no latency of its own.
const appendPoll = 2 * time.Millisecond

// Session is one open stream of audio.
//
// The fields divide into three groups, and the division is the whole
// concurrency argument. The stream and everything describing the current
// utterance belong to the engine's loop goroutine and are never touched
// elsewhere. The queue, the two request flags and lastAudio are written by
// whichever goroutine is reading the client's socket, under the mutex or
// atomically. The event channel is written by the loop and read by the
// client's writer.
type Session struct {
	engine *Engine
	id     string
	params core.RealtimeParams
	events chan core.RealtimeEvent

	// --- loop goroutine only ---
	stream      asr.Stream
	autoTurns   bool
	started     time.Time
	item        string
	items       int
	itemStartMS int64
	audioMS     int64
	rate        int
	text        string
	spoke       bool
	lastPartial time.Time
	chanClosed  bool
	// committing is a commit that has been accepted and is waiting for the
	// audio before it to finish decoding.
	committing bool
	// failed marks a session the loop should tear down at the next step,
	// because an event it had to deliver could not be.
	failed bool

	// --- shared ---
	mu             sync.Mutex
	pending        []chunk
	pendingSamples int
	commit         bool
	clear          bool
	lastAudio      atomic.Int64 // unix nanoseconds
	closed         atomic.Bool
	closeOnce      sync.Once
	dropped        atomic.Int64
}

type chunk struct {
	rate    int
	samples []float32
}

func newSession(e *Engine, stream asr.Stream, p core.RealtimeParams) *Session {
	now := e.now()
	s := &Session{
		engine:    e,
		id:        newID("sess"),
		params:    p,
		events:    make(chan core.RealtimeEvent, eventBuffer),
		stream:    stream,
		autoTurns: p.AutoTurns,
		started:   now,
		rate:      e.rec.SampleRate(),
	}
	s.lastAudio.Store(now.UnixNano())
	s.nextItem(0)
	return s
}

func (s *Session) ID() string                        { return s.id }
func (s *Session) Events() <-chan core.RealtimeEvent { return s.events }

// Append queues audio for the decoder loop.
//
// It waits for room rather than failing the moment the buffer is full, and the
// difference matters more than it looks. A client that sends faster than real
// time is not a client the server cannot keep up with — transcribing a file
// through the realtime API is a reasonable thing to do, and a streaming model
// decodes at a fraction of real time, so the queue drains in milliseconds.
// Blocking here stops this connection being read, which stops the client's
// socket draining, which throttles it to decode speed: the flow control a
// websocket already has, used for what it is for.
//
// Only when the buffer stays full for the whole timeout — the decoder is stuck,
// or hopelessly behind — does it give up and say so. Returning an error is
// still better than dropping audio: a hole in a transcript that nothing
// reported would leave the caller believing the missing words were never said.
func (s *Session) Append(sampleRate int, samples []float32) error {
	if len(samples) == 0 {
		return nil
	}
	if sampleRate <= 0 {
		return core.Errorf(core.CodeInvalidRequest, "audio sample rate must be positive")
	}

	limit := s.engine.opt.MaxBufferedSeconds * sampleRate
	if len(samples) > limit {
		// One chunk larger than the whole budget would wait for ever.
		return core.Errorf(core.CodeInvalidRequest,
			"this frame carries %.1f seconds of audio, more than the %d second buffer; "+
				"send it in smaller pieces",
			float64(len(samples))/float64(sampleRate), s.engine.opt.MaxBufferedSeconds)
	}

	deadline := s.engine.now().Add(s.engine.appendTimeout)
	for {
		if s.closed.Load() {
			return core.Errorf(core.CodeInvalidRequest, "this realtime session is closed")
		}

		s.mu.Lock()
		if s.pendingSamples+len(samples) <= limit {
			// The slice is the caller's; the loop reads it later, so it is
			// copied rather than retained. Reusing a decode buffer is normal
			// on a hot path and the resulting corruption would be invisible.
			s.pending = append(s.pending,
				chunk{rate: sampleRate, samples: append([]float32(nil), samples...)})
			s.pendingSamples += len(samples)
			s.mu.Unlock()

			s.lastAudio.Store(s.engine.now().UnixNano())
			s.engine.poke()
			return nil
		}
		s.mu.Unlock()

		if !s.engine.now().Before(deadline) {
			return core.Errorf(core.CodeQueueFull,
				"%d seconds of audio have been waiting to be recognised for %s: "+
					"the server is not keeping up with this stream",
				s.engine.opt.MaxBufferedSeconds, s.engine.appendTimeout)
		}
		// Waking the loop on every attempt, because the audio already queued
		// is what has to drain before this chunk fits.
		s.engine.poke()
		time.Sleep(appendPoll)
	}
}

// Commit ends the current utterance now.
func (s *Session) Commit() {
	if s.closed.Load() {
		return
	}
	s.mu.Lock()
	s.commit = true
	s.mu.Unlock()
	s.engine.poke()
}

// Clear discards audio that has not been recognised yet.
func (s *Session) Clear() {
	if s.closed.Load() {
		return
	}
	s.mu.Lock()
	s.pending = nil
	s.pendingSamples = 0
	// A commit that has not been acted on is dropped with the audio it was
	// about; otherwise clearing the buffer would still produce a transcript of
	// it.
	s.commit = false
	s.clear = true
	s.mu.Unlock()
	s.engine.poke()
}

// Close ends the session. The event channel closes once the loop has let go of
// the stream, so a reader draining events still sees everything already
// emitted.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		select {
		case s.engine.close <- s:
		case <-s.engine.done:
		}
	})
	return nil
}

// Dropped is how many partial hypotheses were discarded because the client was
// not reading them. Reported in the log when a session ends, as the one honest
// signal that a client cannot keep up.
func (s *Session) Dropped() int64 { return s.dropped.Load() }

// feed moves queued audio into the recogniser. Loop goroutine only.
func (s *Session) feed() {
	s.mu.Lock()
	queued := s.pending
	s.pending = nil
	s.pendingSamples = 0
	s.mu.Unlock()

	for _, c := range queued {
		s.rate = c.rate
		s.stream.Accept(c.rate, c.samples)
		// The timeline is in milliseconds of audio the client sent, not of
		// audio the model consumed: it is what a client can line its own
		// recording up against.
		s.audioMS += int64(len(c.samples)) * 1000 / int64(c.rate)
	}
}

// takeCommit and takeClear read and clear the request flags. Loop goroutine.
func (s *Session) takeCommit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	was := s.commit
	s.commit = false
	return was
}

func (s *Session) takeClear() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	was := s.clear
	s.clear = false
	return was
}

func (s *Session) lastAudioAt() time.Time { return time.Unix(0, s.lastAudio.Load()) }

// nextItem starts a new utterance at startMS. Loop goroutine only.
func (s *Session) nextItem(startMS int64) {
	s.items++
	s.item = fmt.Sprintf("%s-item-%d", s.id, s.items)
	s.itemStartMS = startMS
	s.text = ""
	s.spoke = false
	s.lastPartial = time.Time{}
}

// emit delivers an event, or decides what to do when it cannot.
func (s *Session) emit(ev core.RealtimeEvent, droppable bool) {
	if s.chanClosed {
		return
	}
	select {
	case s.events <- ev:
	default:
		if droppable {
			s.dropped.Add(1)
			return
		}
		s.failed = true
	}
}

// finishChannel closes the event channel. Loop goroutine only, and once.
func (s *Session) finishChannel() {
	if s.chanClosed {
		return
	}
	s.chanClosed = true
	s.closed.Store(true)
	close(s.events)
}

// assemble turns token timings into words, when the model produced any.
func (s *Session) assemble(h asr.Hypothesis, endMS int64) []core.Word {
	if len(h.Timestamps) == 0 {
		return nil
	}
	return words.Assemble(
		asr.Recognition{Text: h.Text, Tokens: h.Tokens, Timestamps: h.Timestamps},
		words.Options{
			ModelingUnit: s.engine.rec.ModelingUnit(),
			SegmentStart: float64(s.itemStartMS) / 1000,
			SegmentEnd:   float64(endMS) / 1000,
		})
}

var _ core.RealtimeSession = (*Session)(nil)

// silence is the tail appended on a commit.
func silence(rate int, seconds float64) []float32 {
	n := int(float64(rate) * seconds)
	if n < 1 {
		return nil
	}
	return make([]float32, n)
}

// trimText normalises a hypothesis for comparison and reporting. Streaming
// models emit leading and trailing spaces as the hypothesis grows, and a
// client should not see a partial change that is only whitespace.
func trimText(s string) string { return strings.TrimSpace(s) }

// delta is what text gained since the previous hypothesis, or empty when the
// recogniser revised rather than extended it. Reporting an empty delta is
// honest; reporting a diff of a retraction as if it were new text is not.
func delta(previous, current string) string {
	if previous != "" && strings.HasPrefix(current, previous) {
		return strings.TrimPrefix(current, previous)
	}
	if previous == "" {
		return current
	}
	return ""
}

// baseID strips the revision from a model key so that a client may name either
// "t-one-ctc-ru" or "t-one-ctc-ru@2025-09-08".
func baseID(key string) string {
	id, _, _ := strings.Cut(key, "@")
	return id
}

// newID is a short random identifier. Random rather than sequential because it
// appears in events a client may log: a counter would leak how busy the server
// is, and it is the same shape the pipeline uses for result ids.
func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}
