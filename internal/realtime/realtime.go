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
	// EndpointSilence is the pause after which the recogniser ends an utterance.
	// The recogniser owns it; the engine only reports it.
	EndpointSilence time.Duration
	// MaxUtterance is the length by which an utterance is closed, measured in
	// audio the client sent since the utterance began. Zero means there is no
	// limit. The engine owns this rather than the recogniser's own endpointer,
	// which cuts at the first decoded chunk past the limit — in the middle of a
	// word, with the model's last half second of output still unsaid.
	MaxUtterance time.Duration
	// PauseSearch is how long before MaxUtterance the engine starts looking for
	// somewhere to close at: the first pause it finds from then on ends the
	// utterance. Zero closes at the limit itself.
	PauseSearch time.Duration
	// MinPause is the quiet that counts as a pause when that search starts. It
	// shrinks towards minDip as the limit approaches, so that a speaker who
	// does not stop is still cut at a breath rather than at a word.
	MinPause time.Duration
	// FlushTail is the silence fed to a stream before an utterance is closed, to
	// make the model say what it has heard and not yet written. A streaming
	// model decodes late: measured on Nemotron, the last word or two of the
	// audio come out only once about half a second more has arrived.
	FlushTail time.Duration
	// LeadSilence is the silence a stream is given after it is reset, before
	// any audio of the next utterance. A cache-aware encoder starts from an empty
	// left context, and speech that begins on its first frame is where it is
	// weakest: Nemotron drops the first word of a clip until about half a second
	// of silence stands in front of it. An utterance that begins at a pause has
	// less than that.
	LeadSilence time.Duration
	// Languages comes from the model manifest, for the session description.
	Languages []string
	// Locales is what a multilingual model offers, for the same description.
	Locales []string
	Logger  *slog.Logger
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
	// defaultFlushTail is how much silence closes an utterance. The feature
	// extractor needs a frame's worth of lookahead to finish the last chunk,
	// and a streaming transducer then needs a chunk of right context before it
	// writes a word it has already heard. Measured on Nemotron's export for the
	// whole-file path (see asr/sherpa/wholefile.go): 0.5 s lost the last word,
	// 0.7 to 1.1 s kept it, and the closing full stop arrived at 1.3 s. The same
	// model, so the same figure.
	defaultFlushTail = 1300 * time.Millisecond
	// defaultLeadSilence is measured too, on the same model: half a second in
	// front of a clip is what stops its first word being dropped.
	defaultLeadSilence = 500 * time.Millisecond
	// flushPiece is how finely the tail is fed, which is what bounds the
	// silence left over in the stream afterwards.
	flushPiece = 20 * time.Millisecond
	// maxFlush bounds the flush however the model behaves, because the loop is
	// shared and an unbounded wait in it is never right.
	maxFlush = 3 * time.Second

	defaultMinPause = 400 * time.Millisecond
	// minDip is the shortest quiet the engine will close an utterance at. A
	// pause shorter than this can sit inside a word — the closure of a stop
	// consonant is as long — and the search never goes below it.
	minDip = 150 * time.Millisecond
	// cutGuard is how long the end of the audio must have been quiet before an
	// endpoint the recogniser raised is believed. The recogniser decides from
	// tokens, and tokens trail the audio by up to a chunk, so for that long
	// after speech starts it still reports silence.
	cutGuard = 700 * time.Millisecond
)

// cutKind says what ends an utterance.
type cutKind uint8

const (
	cutNone cutKind = iota
	// cutSilence is the recogniser's endpointer, after the speaker stopped.
	cutSilence
	// cutPause is the engine closing a long utterance at a pause.
	cutPause
	// cutLimit is the engine closing one at the limit, with no pause to be
	// found.
	cutLimit
	// cutCommit is the client.
	cutCommit
)

func (k cutKind) String() string {
	switch k {
	case cutSilence:
		return "silence"
	case cutPause:
		return "pause"
	case cutLimit:
		return "max_length"
	case cutCommit:
		return "commit"
	}
	return ""
}

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
	if o.FlushTail <= 0 {
		o.FlushTail = defaultFlushTail
	}
	if o.LeadSilence < 0 {
		o.LeadSilence = 0
	} else if o.LeadSilence == 0 {
		o.LeadSilence = defaultLeadSilence
	}
	if o.MaxUtterance < 0 {
		o.MaxUtterance = 0
	}
	// The search has to fit inside the limit, and a pause too short to be one
	// would cut words in half.
	o.PauseSearch = min(max(o.PauseSearch, 0), o.MaxUtterance)
	if o.PauseSearch > 0 {
		if o.MinPause <= 0 {
			o.MinPause = defaultMinPause
		}
		o.MinPause = max(o.MinPause, minDip)
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
		Locales:        e.opt.Locales,
		SampleRate:     e.rec.SampleRate(),
		Punctuation:    caps.PunctuationBuiltin,
		WordTimestamps: caps.WordTimestamps,
		AutoTurns:      true,
		MaxSessions:    e.opt.MaxSessions,
		Active:         e.Active(),
		Capabilities:   caps,
		Endpointing: core.RealtimeEndpointing{
			SilenceMS:      int(e.opt.EndpointSilence.Milliseconds()),
			MaxUtteranceMS: int(e.opt.MaxUtterance.Milliseconds()),
			PauseSearchMS:  int(e.opt.PauseSearch.Milliseconds()),
			MinPauseMS:     int(e.opt.MinPause.Milliseconds()),
		},
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
	s := newSession(e, stream, p)
	s.applyLanguage(p.Language)
	e.prime(s)
	return s, nil
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
		if lang, changed := s.takeLanguage(); changed {
			s.applyLanguage(lang)
		}
		if s.takeClear() {
			e.resetStream(s)
			// Whatever was closing the utterance was about audio that has just
			// been thrown away.
			s.closing = cutNone
			end := s.audioMS
			s.emit(core.RealtimeEvent{Kind: core.RealtimeCleared, Item: s.item, EndMS: end}, false)
			s.nextItem(end)
		}
	}

	// One round at a time, looking at each stream before the next: an
	// endpoint is a flag the recogniser raises and keeps raised until the
	// stream is reset, so decoding several chunks before checking would
	// append the start of the next utterance to the one that just ended.
	// A client that sends audio in bursts — or a server catching up after a
	// buffer built up — would get two sentences run together.
	//
	// The same is why audio goes in only as it is wanted. Each round gives a
	// stream just enough to have something to decode, so that what the engine
	// knows about the audio — how loud it was, how long the utterance has run —
	// is about the point the decoder has reached, and not about the end of a
	// burst that the decoder is seconds behind. Everything that follows is a
	// decision about a boundary, and a decision about the wrong moment loses
	// the word that was there.
	for round := 0; round < maxRoundsPerTick; round++ {
		for s := range sessions {
			// An utterance that is being closed takes no more audio: what has
			// arrived since belongs to the next one, and stays queued until
			// this one is finished.
			if s.closing == cutNone {
				e.feedUntilReady(s)
			}
		}
		decoded := e.decodeRound(sessions)
		if decoded == 0 {
			break
		}
		for _, s := range e.ready[:decoded] {
			e.observe(s)
		}
	}

	// Closings last, and only once everything the client sent before them has
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
		if s.closing == cutNone && s.takeCommit() {
			// Everything queued before the commit has been fed — the stream
			// is never given what came after it — so what is left is to say
			// what it heard and finish.
			s.closing = cutCommit
		}
		if s.closing != cutNone {
			e.closeItem(s)
		}
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

	// An utterance that is already being closed does not also end on its own.
	if !s.autoTurns || s.closing != cutNone {
		return
	}
	if s.stream.Endpoint() {
		if text == "" {
			e.skipSilence(s)
			return
		}
		if !e.holdEndpoint(s) {
			e.finishItem(s, cutSilence)
			return
		}
	} else {
		s.holding = false
	}
}

// skipSilence is what an endpoint with nothing decoded gets.
//
// That endpoint is the recogniser's rule for a long silence, and there is
// nothing in it to end: no utterance to close, no event to send. The stream is
// left alone. It has been listening to silence, which is the best state to hear
// the next speaker from, and resetting it would throw away a word that has
// begun and not yet been written — the first word after a silence is the one a
// reset on a long pause used to take. What moves is where the item says it
// begins, so that its start is near the speech and not at the beginning of the
// silence.
func (e *Engine) skipSilence(s *Session) {
	if quiet, ok := s.meter.quietFor(cutGuard); ok && quiet < cutGuard {
		// Something is arriving: leave the start where it is.
		return
	}
	s.itemStartMS = max(s.itemStartMS, s.audioMS-cutGuard.Milliseconds())
}

// feedUntilReady gives a stream audio until it has enough to decode, stopping
// early where the engine decides the utterance should end. Loop goroutine only.
func (e *Engine) feedUntilReady(s *Session) {
	for !s.stream.Ready() {
		if !s.feedPiece() {
			return
		}
		if kind := e.cutDue(s); kind != cutNone {
			s.closing = kind
			return
		}
	}
}

// maxHold is the longest an endpoint is put off. The usual reason to put one
// off clears within a chunk, as the first token of the speech arrives; this is
// the bound for the case where the meter is wrong about the line, which must
// cost a little latency and not an utterance that never ends.
const maxHold = 1500 * time.Millisecond

// holdEndpoint reports whether the recogniser's endpoint should be put off
// because speech is still arriving.
//
// The endpointer counts blanks the decoder emitted. A streaming model emits
// late, so for up to a chunk after a word begins it has emitted nothing for
// it, and a pause that is really the speaker drawing breath before the next
// sentence looks, to the endpointer, like one that has lasted long enough.
// Resetting the stream then discards audio the model has already consumed and
// not yet turned into words: the first word of the next utterance is gone, and
// nothing in the transcript says so.
//
// The meter has no such lag. If the end of the audio is not quiet, the
// endpoint is not a pause yet. Audio is fed only as the decoder wants it (see
// step), so the end of what the meter has heard is where the decoder is, not a
// burst ahead of it.
func (e *Engine) holdEndpoint(s *Session) bool {
	quiet, ok := s.meter.quietFor(cutGuard)
	if !ok || quiet >= cutGuard {
		return false
	}
	if !s.holding {
		s.holding = true
		s.holdFromMS = s.audioMS
	}
	return time.Duration(s.audioMS-s.holdFromMS)*time.Millisecond < maxHold
}

// cutDue is called after each piece of audio is fed, and says whether the
// utterance should now be closed by the engine.
//
// Past MaxUtterance the answer is always yes. In the PauseSearch before that it
// is yes at the first pause, and the pause that counts shrinks as the limit
// nears: a speaker who is going to be cut might as well be cut at a breath, but
// not at a pause so short it is the gap inside a word.
func (e *Engine) cutDue(s *Session) cutKind {
	limit := e.opt.MaxUtterance
	if limit <= 0 || !s.autoTurns {
		return cutNone
	}
	length := time.Duration(s.audioMS-s.itemStartMS) * time.Millisecond
	if length >= limit {
		return cutLimit
	}

	// A pause is only worth cutting at if something was said before it.
	search := e.opt.PauseSearch
	into := length - (limit - search)
	if search <= 0 || into < 0 || !s.spoke {
		return cutNone
	}
	progress := min(float64(into)/float64(search), 1)
	need := e.opt.MinPause - time.Duration(progress*float64(e.opt.MinPause-minDip))
	if quiet, ok := s.meter.quietFor(need); ok && quiet >= need {
		return cutPause
	}
	return cutNone
}

// closeItem moves the closing of an utterance forward by what one tick allows:
// decode what the stream still holds, then make the model write what it has
// heard, then finish the item. Loop goroutine only.
func (e *Engine) closeItem(s *Session) {
	for rounds := 0; rounds < maxRoundsPerTick && s.stream.Ready(); rounds++ {
		e.rec.Decode([]asr.Stream{s.stream})
		e.observe(s)
	}
	if s.stream.Ready() {
		// More to decode; finish the closing on a later tick.
		return
	}

	e.flush(s)
	kind := s.closing
	s.closing = cutNone
	e.finishItem(s, kind)
}

// flush feeds the stream silence until the model has said everything it heard.
//
// A streaming transducer writes a word some time after it has heard it: on
// Nemotron the last word or two of a clip are not in the hypothesis until half
// a second more audio has arrived, however long the clip was decoded for. An
// utterance that ends without that audio loses those words — and when the end
// is a reset, so does the next one, because the audio they were in has been
// consumed. This is the tail a commit has always fed; it is what every other
// way of ending an utterance was missing.
//
// The silence goes in a piece at a time, each followed by a decode, and stops
// right after a decode. The stream then holds less than one piece of undecoded
// silence. Fed in one block, it would keep up to a chunk of it, ahead of the
// next utterance, and every word time in that utterance would be late by as
// much.
func (e *Engine) flush(s *Session) {
	piece := silence(s.rate, flushPiece.Seconds())
	if piece == nil {
		return
	}
	for fed := time.Duration(0); fed < maxFlush; {
		s.stream.Accept(s.rate, piece)
		fed += flushPiece

		decoded := false
		for rounds := 0; rounds < maxRoundsPerTick && s.stream.Ready(); rounds++ {
			e.rec.Decode([]asr.Stream{s.stream})
			decoded = true
		}
		if fed >= e.opt.FlushTail && decoded {
			return
		}
	}
}

// finishItem closes the current utterance and opens the next.
//
// A commit is a client's request rather than detected silence. The difference
// is what happens when there is no text: silence that produced nothing is not
// an event, but a client that asked for a result is answered even if the answer
// is empty, because it is waiting for one.
func (e *Engine) finishItem(s *Session, kind cutKind) {
	h := s.stream.Hypothesis()
	text := trimText(h.Text)
	end := s.audioMS

	if text != "" && !s.spoke {
		// Everything came out in the flush: say that speech began before saying
		// it ended.
		s.emit(core.RealtimeEvent{
			Kind: core.RealtimeSpeechStarted, Item: s.item, StartMS: s.itemStartMS,
		}, false)
	}

	if text != "" || kind == cutCommit {
		s.emit(core.RealtimeEvent{
			Kind: core.RealtimeSpeechStopped, Item: s.item,
			StartMS: s.itemStartMS, EndMS: end, Cut: kind.String(),
		}, false)
		s.emit(core.RealtimeEvent{
			Kind: core.RealtimeFinal, Item: s.item, Text: text,
			StartMS: s.itemStartMS, EndMS: end, Cut: kind.String(),
			Words: s.assemble(h, end),
		}, false)
	}

	e.resetStream(s)
	s.nextItem(end)
}

// resetStream starts the stream's next utterance: the decoder's state is
// cleared, the language is told to it again, and it is given the silence that
// stands in front of the audio to come. Loop goroutine only.
func (e *Engine) resetStream(s *Session) {
	s.stream.Reset()
	s.applyLanguage(s.language)
	e.prime(s)
}

// prime gives a fresh stream its lead silence. The silence is not part of the
// client's audio and is not on its timeline: word times are shifted back by as
// much (see Session.assemble), so they stay in the client's.
func (e *Engine) prime(s *Session) {
	// The stream's clock starts at the silence, before the audio.
	s.clockMS = s.audioMS - e.opt.LeadSilence.Milliseconds()
	if lead := e.opt.LeadSilence; lead > 0 {
		s.stream.Accept(s.rate, silence(s.rate, lead.Seconds()))
	}
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
