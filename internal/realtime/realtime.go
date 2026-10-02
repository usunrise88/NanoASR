// Package realtime recognises audio while it is still arriving.
//
// The shape of it is one goroutine per engine, not one per session. Everything
// that touches the recogniser — accepting audio, deciding whether a stream has
// enough to decode, decoding, reading the hypothesis — happens on that single
// goroutine, for two reasons.
//
// The first is safety. A sherpa-onnx stream holds the feature extractor's
// buffer and the decoder's state, and both Accept and Decode write to them; a
// writer goroutine per connection would be a data race in C++ that Go's race
// detector cannot see. Confining every call to one goroutine makes the
// question moot.
//
// The second is throughput. Sessions that are ready at the same moment are
// decoded in one batched call, which is what makes the arithmetic worth sending
// to a GPU: a single 16 kHz stream asks for a few milliseconds of work every
// hundred milliseconds and could never keep an accelerator busy. The cost is
// that one engine is one decoder's worth of CPU — asr.inference_slots of them
// — which is the figure realtime.max_sessions has to be chosen against.
package realtime

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/core"
)

// Options configure the engine. Everything here is a bound on what one server
// will do; the per-session choices are in core.RealtimeParams.
type Options struct {
	MaxSessions        int
	MaxSessionDuration time.Duration
	IdleTimeout        time.Duration
	PartialInterval    time.Duration
	MaxBufferedSeconds int
	BatchSize          int
	// Tick is how often the decoder loop runs. It bounds the latency a client
	// sees on top of the model's own chunk size, and it is not a knob an
	// operator has: 20 ms is below the shortest chunk any streaming model
	// uses, so a smaller value buys nothing and a larger one is latency.
	Tick time.Duration
	// Languages comes from the model manifest, for the session description.
	Languages []string
	Logger    *slog.Logger
}

const (
	defaultTick = 20 * time.Millisecond
	// maxRoundsPerTick bounds catch-up work. A client may have buffered
	// several seconds — uploading a file through a socket meant for a
	// microphone is a thing people do — and decoding all of it in one tick
	// would stall every other session and every new connection. Eight rounds
	// is of the order of a hundred times real time, which catches up fast
	// enough that nobody notices and keeps the loop answering.
	maxRoundsPerTick = 8
	// appendTimeout is how long Append waits for a full buffer to drain before
	// it gives up. Generous on purpose: it is not a latency budget, it is the
	// point at which a stalled decoder is admitted to be stalled.
	appendTimeout = 30 * time.Second
	// commitTailSeconds is the silence appended when a client commits the
	// buffer. The feature extractor needs a frame's worth of lookahead to
	// finish the last chunk, so without a tail the final few words of a
	// committed utterance would never be decoded.
	commitTailSeconds = 0.5
)

func (o Options) withDefaults() Options {
	if o.MaxSessions < 1 {
		o.MaxSessions = 4
	}
	if o.BatchSize < 1 {
		o.BatchSize = 8
	}
	if o.Tick <= 0 {
		o.Tick = defaultTick
	}
	if o.PartialInterval < 0 {
		o.PartialInterval = 0
	}
	if o.MaxBufferedSeconds < 1 {
		o.MaxBufferedSeconds = 10
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return o
}

// Engine owns one loaded streaming recogniser and the sessions on it.
type Engine struct {
	rec asr.Streaming
	opt Options
	log *slog.Logger

	open  chan *openRequest
	close chan *Session
	// wake shortens the wait for work that should not sit until the next tick:
	// a commit, a clear, or the first audio of a session.
	wake chan struct{}
	// done is closed when Run returns, so Open can fail instead of blocking
	// against an engine that is no longer there.
	done chan struct{}

	// ready and batch are scratch, reused by the loop goroutine.
	ready []*Session
	batch []asr.Stream

	now func() time.Time
	// appendTimeout is a field rather than the constant so that a test can
	// stop waiting thirty seconds to observe the refusal.
	appendTimeout time.Duration

	// active mirrors len(sessions), which the loop goroutine owns, so that it
	// can be read from anywhere.
	active atomic.Int64
}

// New builds an engine over an already-loaded streaming recogniser. The engine
// does nothing until Run is called.
func New(rec asr.Streaming, opt Options) *Engine {
	opt = opt.withDefaults()
	return &Engine{
		rec:           rec,
		opt:           opt,
		log:           opt.Logger,
		open:          make(chan *openRequest),
		close:         make(chan *Session, 8),
		wake:          make(chan struct{}, 1),
		done:          make(chan struct{}),
		now:           time.Now,
		appendTimeout: appendTimeout,
	}
}

// Describe reports the model and the limits.
func (e *Engine) Describe() core.RealtimeInfo {
	caps := e.rec.Capabilities()
	return core.RealtimeInfo{
		Model:          e.rec.ModelID(),
		Languages:      e.opt.Languages,
		SampleRate:     e.rec.SampleRate(),
		Punctuation:    caps.PunctuationBuiltin,
		WordTimestamps: caps.WordTimestamps,
		AutoTurns:      true,
		MaxSessions:    e.opt.MaxSessions,
		Active:         e.Active(),
		Capabilities:   caps,
	}
}

// Active is the number of open sessions.
//
// Read from a counter rather than asked of the loop goroutine: it is called
// from request handling — the documentation page reports it, and so does every
// session acknowledgement — and a question that waits for a decode to finish
// has no business on that path.
func (e *Engine) Active() int { return int(e.active.Load()) }

type openRequest struct {
	params core.RealtimeParams
	reply  chan openResult
}

type openResult struct {
	session *Session
	err     error
}

// Open starts a session.
func (e *Engine) Open(ctx context.Context, p core.RealtimeParams) (core.RealtimeSession, error) {
	req := &openRequest{params: p, reply: make(chan openResult, 1)}
	select {
	case e.open <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.done:
		return nil, core.Errorf(core.CodeDraining, "the realtime engine is shutting down")
	}

	select {
	case r := <-req.reply:
		return r.session, r.err
	case <-ctx.Done():
		// The loop may already be building a session for a caller who has gone
		// away. Collect it rather than leaving it to hold a slot for ever.
		go e.discard(req.reply)
		return nil, ctx.Err()
	case <-e.done:
		return nil, core.Errorf(core.CodeDraining, "the realtime engine is shutting down")
	}
}

func (e *Engine) discard(reply chan openResult) {
	select {
	case r := <-reply:
		if r.session != nil {
			_ = r.session.Close()
		}
	case <-e.done:
	case <-time.After(30 * time.Second):
	}
}

// Run is the decoder loop. It returns when ctx is cancelled, having closed
// every session. The recogniser itself is the caller's to close.
func (e *Engine) Run(ctx context.Context) {
	defer close(e.done)

	ticker := time.NewTicker(e.opt.Tick)
	defer ticker.Stop()

	sessions := map[*Session]struct{}{}
	defer func() {
		for s := range sessions {
			e.teardown(s, core.Errorf(core.CodeDraining, "the server is shutting down"))
		}
		e.active.Store(0)
	}()

	for {
		select {
		case <-ctx.Done():
			return

		case req := <-e.open:
			s, err := e.admit(sessions, req.params)
			if err != nil {
				req.reply <- openResult{err: err}
				continue
			}
			sessions[s] = struct{}{}
			e.active.Store(int64(len(sessions)))
			req.reply <- openResult{session: s}

		case s := <-e.close:
			if _, live := sessions[s]; live {
				delete(sessions, s)
				e.active.Store(int64(len(sessions)))
				e.teardown(s, nil)
			}

		case <-e.wake:
			e.step(sessions)

		case now := <-ticker.C:
			e.step(sessions)
			e.expire(sessions, now)
		}
	}
}

// admit builds a session, or explains why not.
func (e *Engine) admit(sessions map[*Session]struct{}, p core.RealtimeParams) (*Session, error) {
	if len(sessions) >= e.opt.MaxSessions {
		return nil, core.Errorf(core.CodeQueueFull,
			"all %d realtime sessions are in use; retry shortly or raise realtime.max_sessions",
			e.opt.MaxSessions)
	}
	if p.ModelID != "" && p.ModelID != e.rec.ModelID() && p.ModelID != baseID(e.rec.ModelID()) {
		return nil, core.Errorf(core.CodeModelNotFound,
			"this server streams with %s; a realtime session cannot choose another model",
			e.rec.ModelID()).WithParam("model")
	}

	stream, err := e.rec.NewStream()
	if err != nil {
		return nil, err
	}
	return newSession(e, stream, p), nil
}

// teardown closes one session's stream and its event channel. It runs on the
// loop goroutine, which is the only one allowed to touch the stream.
func (e *Engine) teardown(s *Session, reason error) {
	if reason != nil {
		s.emit(core.RealtimeEvent{Kind: core.RealtimeError, Item: s.item, Err: reason}, false)
	}
	s.stream.Close()
	s.finishChannel()
}

// step feeds every session the audio it has queued, decodes whatever is ready,
// and reports what changed.
func (e *Engine) step(sessions map[*Session]struct{}) {
	if len(sessions) == 0 {
		return
	}

	for s := range sessions {
		if s.takeClear() {
			s.stream.Reset()
			end := s.audioMS
			s.emit(core.RealtimeEvent{Kind: core.RealtimeCleared, Item: s.item, EndMS: end}, false)
			s.nextItem(end)
		}
		s.feed()
	}

	// One round at a time, looking at each stream before the next: an
	// endpoint is a flag the recogniser raises and keeps raised until the
	// stream is reset, so decoding several chunks before checking would
	// append the start of the next utterance to the one that just ended.
	// A client that sends audio in bursts — or a server catching up after a
	// buffer built up — would get two sentences run together.
	for round := 0; round < maxRoundsPerTick; round++ {
		decoded := e.decodeRound(sessions)
		if decoded == 0 {
			break
		}
		for _, s := range e.ready[:decoded] {
			e.observe(s)
		}
	}

	// Commits last, and only once everything the client sent before it has
	// been decoded: a commit means "all of that was one utterance", so
	// finishing the item while audio is still undecoded truncates the
	// transcript. A client that uploaded faster than real time — which the
	// throttling in Append allows — can easily have several seconds queued
	// at this point.
	//
	// The waiting is spread over ticks rather than done in one: decoding ten
	// seconds of backlog here would stall every other session and every new
	// connection for as long as it took.
	for s := range sessions {
		if s.takeCommit() {
			s.committing = true
		}
		if !s.committing {
			continue
		}
		for rounds := 0; rounds < maxRoundsPerTick && s.stream.Ready(); rounds++ {
			e.rec.Decode([]asr.Stream{s.stream})
			e.observe(s)
		}
		if s.stream.Ready() {
			// More to decode; finish the commit on a later tick.
			continue
		}

		// The feature extractor needs a frame's worth of lookahead to finish
		// the last chunk, so the tail is flushed with silence. Two or three
		// rounds at any model's chunk size; the cap is there because an
		// unbounded loop in the shared goroutine is never right.
		s.stream.Accept(s.rate, silence(s.rate, commitTailSeconds))
		for rounds := 0; rounds < maxRoundsPerTick && s.stream.Ready(); rounds++ {
			e.rec.Decode([]asr.Stream{s.stream})
		}
		s.committing = false
		e.finishItem(s, true)
	}

	// A session whose client stopped reading is closed here rather than left
	// holding a slot: emit could not deliver an event that mattered, and
	// there is nothing useful left to do with the connection.
	for s := range sessions {
		if !s.failed {
			continue
		}
		e.log.Warn("closing a realtime session that is not reading its events",
			"session", s.ID(), "dropped_partials", s.Dropped())
		delete(sessions, s)
		e.active.Store(int64(len(sessions)))
		e.teardown(s, core.Errorf(core.CodeQueueFull,
			"the client is not reading its own transcript fast enough"))
	}
}

// decodeRound advances every stream that has enough frames by one chunk, in
// batches of at most BatchSize, and returns how many were decoded. The streams
// it decoded are left in e.ready for the caller to inspect.
func (e *Engine) decodeRound(sessions map[*Session]struct{}) int {
	e.ready = e.ready[:0]
	for s := range sessions {
		if s.stream.Ready() {
			e.ready = append(e.ready, s)
		}
	}
	for rest := e.ready; len(rest) > 0; {
		n := min(len(rest), e.opt.BatchSize)
		e.batch = e.batch[:0]
		for _, s := range rest[:n] {
			e.batch = append(e.batch, s.stream)
		}
		e.rec.Decode(e.batch)
		rest = rest[n:]
	}
	return len(e.ready)
}

// observe turns a changed hypothesis into events.
func (e *Engine) observe(s *Session) {
	h := s.stream.Hypothesis()
	text := trimText(h.Text)
	now := e.now()

	if text != "" && !s.spoke {
		s.spoke = true
		s.emit(core.RealtimeEvent{
			Kind: core.RealtimeSpeechStarted, Item: s.item, StartMS: s.itemStartMS,
		}, false)
	}

	if text != s.text && now.Sub(s.lastPartial) >= e.opt.PartialInterval {
		ev := core.RealtimeEvent{
			Kind: core.RealtimePartial, Item: s.item, Text: text, StartMS: s.itemStartMS,
			Delta: delta(s.text, text),
		}
		s.text = text
		s.lastPartial = now
		// Partials are the one droppable event: a client too slow to read them
		// is better served by the next one, which supersedes it anyway.
		s.emit(ev, true)
	}

	if s.autoTurns && s.stream.Endpoint() {
		e.finishItem(s, false)
	}
}

// finishItem closes the current utterance and opens the next.
//
// forced is a client's commit rather than detected silence. The difference is
// what happens when there is no text: silence that produced nothing is not an
// event, but a client that asked for a result is answered even if the answer is
// empty, because it is waiting for one.
func (e *Engine) finishItem(s *Session, forced bool) {
	h := s.stream.Hypothesis()
	text := trimText(h.Text)
	end := s.audioMS

	if text != "" || forced {
		s.emit(core.RealtimeEvent{
			Kind: core.RealtimeSpeechStopped, Item: s.item,
			StartMS: s.itemStartMS, EndMS: end,
		}, false)
		s.emit(core.RealtimeEvent{
			Kind: core.RealtimeFinal, Item: s.item, Text: text,
			StartMS: s.itemStartMS, EndMS: end,
			Words: s.assemble(h, end),
		}, false)
	}

	s.stream.Reset()
	s.nextItem(end)
}

// expire closes sessions that have outstayed their limits.
func (e *Engine) expire(sessions map[*Session]struct{}, now time.Time) {
	for s := range sessions {
		var reason error
		switch {
		case e.opt.MaxSessionDuration > 0 && now.Sub(s.started) > e.opt.MaxSessionDuration:
			reason = core.Errorf(core.CodeProcessingTimeout,
				"this session reached the %s limit on one connection", e.opt.MaxSessionDuration)
		case e.opt.IdleTimeout > 0 && now.Sub(s.lastAudioAt()) > e.opt.IdleTimeout:
			reason = core.Errorf(core.CodeProcessingTimeout,
				"no audio for %s: closing the session", e.opt.IdleTimeout)
		default:
			continue
		}
		delete(sessions, s)
		e.active.Store(int64(len(sessions)))
		e.teardown(s, reason)
	}
}

// poke asks the loop to run a step now rather than at the next tick. It never
// blocks: a wake already pending does the same job.
func (e *Engine) poke() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

var _ core.RealtimeService = (*Engine)(nil)
