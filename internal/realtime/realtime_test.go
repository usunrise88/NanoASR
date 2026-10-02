package realtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

// start runs an engine for the duration of the test. tweak, when given, adjusts
// the engine before the loop is running, which is the only time its unexported
// fields can be written without racing it.
func start(t *testing.T, rec *fakeRecognizer, opt Options, tweak ...func(*Engine)) *Engine {
	t.Helper()
	if opt.Tick == 0 {
		opt.Tick = 2 * time.Millisecond
	}
	e := New(rec, opt)
	for _, f := range tweak {
		f(e)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		e.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Error("the engine did not stop")
		}
	})
	return e
}

func open(t *testing.T, e *Engine, p core.RealtimeParams) core.RealtimeSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := e.Open(ctx, p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// speak feeds seconds of audio in 100 ms chunks, the way a client would.
func speak(t *testing.T, s core.RealtimeSession, rate int, seconds float64) {
	t.Helper()
	chunk := rate / 10
	for sent := 0.0; sent < seconds; sent += 0.1 {
		if err := s.Append(rate, make([]float32, chunk)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
}

// collect reads events until one of kind stop arrives or time runs out.
func collect(t *testing.T, s core.RealtimeSession, stop core.RealtimeEventKind) []core.RealtimeEvent {
	t.Helper()
	deadline := time.After(5 * time.Second)
	var out []core.RealtimeEvent
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
			if ev.Kind == stop {
				return out
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s; got %s", stop, kinds(out))
			return out
		}
	}
}

func kinds(evs []core.RealtimeEvent) string {
	var out []string
	for _, ev := range evs {
		out = append(out, string(ev.Kind))
	}
	return strings.Join(out, ",")
}

func find(evs []core.RealtimeEvent, kind core.RealtimeEventKind) (core.RealtimeEvent, bool) {
	for _, ev := range evs {
		if ev.Kind == kind {
			return ev, true
		}
	}
	return core.RealtimeEvent{}, false
}

func TestASessionReportsSpeechThenPartialsThenAFinalTranscript(t *testing.T) {
	rec := newFake()
	rec.endpointAfter = 3
	e := start(t, rec, Options{MaxSessions: 2})

	s := open(t, e, core.RealtimeParams{AutoTurns: true})
	defer s.Close()

	speak(t, s, 16000, 0.5)
	evs := collect(t, s, core.RealtimeFinal)

	if _, ok := find(evs, core.RealtimeSpeechStarted); !ok {
		t.Errorf("no speech_started in %s", kinds(evs))
	}
	if _, ok := find(evs, core.RealtimePartial); !ok {
		t.Errorf("no partial in %s", kinds(evs))
	}
	stopped, ok := find(evs, core.RealtimeSpeechStopped)
	if !ok {
		t.Fatalf("no speech_stopped in %s", kinds(evs))
	}
	final, _ := find(evs, core.RealtimeFinal)
	if final.Text != "раз два три" {
		t.Errorf("final transcript is %q, want %q", final.Text, "раз два три")
	}
	if final.Item == "" || final.Item != stopped.Item {
		t.Errorf("speech_stopped and final disagree about the item: %q vs %q", stopped.Item, final.Item)
	}
	// 500 ms of audio was fed before the endpoint, so the utterance cannot
	// claim to have ended later than that.
	if final.EndMS <= 0 || final.EndMS > 500 {
		t.Errorf("final EndMS is %d, want 0 < EndMS <= 500", final.EndMS)
	}
	if len(final.Words) == 0 {
		t.Error("final carries no words although the model reports timestamps")
	}
}

func TestEachUtteranceGetsItsOwnItemID(t *testing.T) {
	rec := newFake()
	rec.endpointAfter = 2
	e := start(t, rec, Options{MaxSessions: 1})

	s := open(t, e, core.RealtimeParams{AutoTurns: true})
	defer s.Close()

	speak(t, s, 16000, 0.3)
	first := collect(t, s, core.RealtimeFinal)
	speak(t, s, 16000, 0.3)
	second := collect(t, s, core.RealtimeFinal)

	a, _ := find(first, core.RealtimeFinal)
	b, _ := find(second, core.RealtimeFinal)
	if a.Item == b.Item {
		t.Errorf("both utterances carry item %q", a.Item)
	}
	// The second utterance starts where the first ended, not at zero: the
	// timeline is the session's, not the item's.
	if b.StartMS < a.EndMS {
		t.Errorf("second utterance starts at %d, before the first ended at %d", b.StartMS, a.EndMS)
	}
}

func TestWithoutAutomaticTurnsOnlyACommitEndsAnUtterance(t *testing.T) {
	rec := newFake()
	// The model would have ended it, and must not be allowed to.
	rec.endpointAfter = 2
	e := start(t, rec, Options{MaxSessions: 1})

	s := open(t, e, core.RealtimeParams{AutoTurns: false})
	defer s.Close()

	speak(t, s, 16000, 0.4)

	// Nothing may finalise on its own. Partials are expected; a final is a bug.
	deadline := time.After(200 * time.Millisecond)
	for done := false; !done; {
		select {
		case ev := <-s.Events():
			if ev.Kind == core.RealtimeFinal {
				t.Fatal("an utterance ended without a commit although turn detection is off")
			}
		case <-deadline:
			done = true
		}
	}

	s.Commit()
	evs := collect(t, s, core.RealtimeFinal)
	final, _ := find(evs, core.RealtimeFinal)
	if final.Text == "" {
		t.Errorf("the committed utterance is empty; events were %s", kinds(evs))
	}
}

func TestACommitAnswersEvenWhenNothingWasSaid(t *testing.T) {
	rec := newFake()
	rec.words = nil // silence: the decoder produces no text at all
	e := start(t, rec, Options{MaxSessions: 1})

	s := open(t, e, core.RealtimeParams{AutoTurns: true})
	defer s.Close()

	speak(t, s, 16000, 0.2)
	s.Commit()

	evs := collect(t, s, core.RealtimeFinal)
	final, ok := find(evs, core.RealtimeFinal)
	if !ok {
		t.Fatalf("a commit produced no final event; got %s", kinds(evs))
	}
	if final.Text != "" {
		t.Errorf("final text is %q, want empty", final.Text)
	}
}

func TestSilenceAloneProducesNoEvents(t *testing.T) {
	rec := newFake()
	rec.words = nil
	rec.endpointAfter = 0
	e := start(t, rec, Options{MaxSessions: 1})

	s := open(t, e, core.RealtimeParams{AutoTurns: true})
	defer s.Close()

	speak(t, s, 16000, 0.5)
	select {
	case ev := <-s.Events():
		t.Errorf("silence produced a %s event", ev.Kind)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestClearDiscardsTheUtteranceInProgress(t *testing.T) {
	rec := newFake()
	e := start(t, rec, Options{MaxSessions: 1})

	s := open(t, e, core.RealtimeParams{AutoTurns: true})
	defer s.Close()

	speak(t, s, 16000, 0.3)
	// Wait for something to have been recognised, so the clear has work to do.
	_ = collect(t, s, core.RealtimePartial)

	s.Clear()
	evs := collect(t, s, core.RealtimeCleared)
	cleared, ok := find(evs, core.RealtimeCleared)
	if !ok {
		t.Fatalf("no cleared event; got %s", kinds(evs))
	}
	if cleared.Item == "" {
		t.Error("the cleared event does not say which item it ended")
	}

	// And the next utterance starts from nothing.
	speak(t, s, 16000, 0.3)
	next := collect(t, s, core.RealtimePartial)
	partial, _ := find(next, core.RealtimePartial)
	if partial.Item == cleared.Item {
		t.Error("recognition continued in the item that was cleared")
	}
}

func TestTheServerRefusesMoreSessionsThanItCanDecode(t *testing.T) {
	rec := newFake()
	e := start(t, rec, Options{MaxSessions: 2})

	a := open(t, e, core.RealtimeParams{})
	defer a.Close()
	b := open(t, e, core.RealtimeParams{})
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := e.Open(ctx, core.RealtimeParams{}); err == nil {
		t.Fatal("a third session was admitted although max_sessions is 2")
	} else if code := codeOf(err); code != core.CodeQueueFull {
		t.Errorf("error code is %s, want %s (%v)", code, core.CodeQueueFull, err)
	}

	// Closing one frees the slot.
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if _, err := e.Open(ctx, core.RealtimeParams{}); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("a slot freed by a closed session never became available")
}

func TestASessionCannotChooseAnotherModel(t *testing.T) {
	rec := newFake()
	e := start(t, rec, Options{MaxSessions: 1})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := e.Open(ctx, core.RealtimeParams{ModelID: "whisper-1"})
	if err == nil {
		t.Fatal("a session asking for another model was admitted")
	}
	if !strings.Contains(err.Error(), "fake-streaming") {
		t.Errorf("the error does not name the loaded model: %v", err)
	}

	// The configured model, with or without its revision, is accepted.
	for _, id := range []string{"fake-streaming", "fake-streaming@1"} {
		s, err := e.Open(ctx, core.RealtimeParams{ModelID: id})
		if err != nil {
			t.Fatalf("Open with model %q: %v", id, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		// Give the loop a moment to release the slot before the next attempt.
		time.Sleep(20 * time.Millisecond)
	}
}

// A client that sends faster than real time is throttled, not disconnected:
// Append waits for the decoder to catch up.
func TestFastAudioIsThrottledRatherThanRefused(t *testing.T) {
	rec := newFake()
	e := start(t, rec, Options{MaxSessions: 1, MaxBufferedSeconds: 1})

	s := open(t, e, core.RealtimeParams{})
	defer s.Close()

	// Five seconds of audio into a one-second buffer, as fast as it goes.
	for range 50 {
		if err := s.Append(16000, make([]float32, 16000/10)); err != nil {
			t.Fatalf("a burst of audio was refused: %v", err)
		}
	}
}

// But a decoder that has stopped is admitted to have stopped.
func TestAudioIsRefusedWhenRecognitionStops(t *testing.T) {
	rec := newFake()
	// A decode that never returns: the queue can then only grow.
	rec.blockDecode = make(chan struct{})
	e := start(t, rec, Options{MaxSessions: 1, MaxBufferedSeconds: 1},
		func(e *Engine) { e.appendTimeout = 100 * time.Millisecond })
	// Registered after start, so it runs before start's cleanup and the engine
	// can finish stopping.
	t.Cleanup(func() { close(rec.blockDecode) })

	s := open(t, e, core.RealtimeParams{})
	defer s.Close()

	var err error
	for range 40 {
		if err = s.Append(16000, make([]float32, 16000/10)); err != nil {
			break
		}
	}
	if err == nil {
		t.Fatal("the session accepted more audio than max_buffered_seconds allows")
	}
	if code := codeOf(err); code != core.CodeQueueFull {
		t.Errorf("error code is %s, want %s (%v)", code, core.CodeQueueFull, err)
	}
	if !strings.Contains(err.Error(), "not keeping up") {
		t.Errorf("the error does not say what went wrong: %v", err)
	}

	// A frame bigger than the whole budget would wait for ever, so it is
	// refused immediately and says why.
	err = s.Append(16000, make([]float32, 16000*5))
	if err == nil || !strings.Contains(err.Error(), "smaller pieces") {
		t.Errorf("an oversized frame was not refused clearly: %v", err)
	}
}

func TestAnIdleSessionIsClosedWithAReason(t *testing.T) {
	rec := newFake()
	e := start(t, rec, Options{MaxSessions: 1, IdleTimeout: 30 * time.Millisecond})

	s := open(t, e, core.RealtimeParams{})
	defer s.Close()

	evs := collect(t, s, core.RealtimeError)
	ev, ok := find(evs, core.RealtimeError)
	if !ok {
		t.Fatalf("an idle session was not closed; got %s", kinds(evs))
	}
	if ev.Err == nil || !strings.Contains(ev.Err.Error(), "no audio") {
		t.Errorf("the closing event does not explain itself: %+v", ev)
	}
	// The channel closes after the error, so a reader learns the session ended.
	select {
	case _, open := <-s.Events():
		if open {
			t.Error("more events arrived after the session was closed")
		}
	case <-time.After(time.Second):
		t.Error("the event channel was not closed")
	}
}

func TestALongSessionIsClosedAtItsLimit(t *testing.T) {
	rec := newFake()
	e := start(t, rec, Options{MaxSessions: 1, MaxSessionDuration: 30 * time.Millisecond})

	s := open(t, e, core.RealtimeParams{})
	defer s.Close()

	evs := collect(t, s, core.RealtimeError)
	ev, _ := find(evs, core.RealtimeError)
	if ev.Err == nil || !strings.Contains(ev.Err.Error(), "limit on one connection") {
		t.Errorf("the closing event does not explain itself: %+v", ev)
	}
}

func TestShutdownClosesEverySession(t *testing.T) {
	rec := newFake()
	e := New(rec, Options{MaxSessions: 2, Tick: 2 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); e.Run(ctx) }()

	s := open(t, e, core.RealtimeParams{})
	cancel()

	evs := collect(t, s, core.RealtimeError)
	ev, ok := find(evs, core.RealtimeError)
	if !ok || ev.Err == nil || codeOf(ev.Err) != core.CodeDraining {
		t.Errorf("a session was not told the server is shutting down: %s", kinds(evs))
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}

	// And a session cannot be opened afterwards.
	if _, err := e.Open(context.Background(), core.RealtimeParams{}); err == nil {
		t.Error("a session was opened on a stopped engine")
	}
}

func TestAppendAfterCloseIsRefused(t *testing.T) {
	rec := newFake()
	e := start(t, rec, Options{MaxSessions: 1})
	s := open(t, e, core.RealtimeParams{})

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(16000, make([]float32, 160)); err == nil {
		t.Error("a closed session accepted audio")
	}
	// Close is idempotent: a dialect may close in a defer and on an error path.
	if err := s.Close(); err != nil {
		t.Errorf("the second Close returned %v", err)
	}
}

func TestDescribeReportsTheLoadedModel(t *testing.T) {
	rec := newFake()
	rec.caps.PunctuationBuiltin = false
	e := start(t, rec, Options{MaxSessions: 3, Languages: []string{"ru"}})

	info := e.Describe()
	if info.Model != "fake-streaming@1" {
		t.Errorf("model is %q", info.Model)
	}
	if info.SampleRate != 16000 || info.MaxSessions != 3 {
		t.Errorf("unexpected description: %+v", info)
	}
	if info.Punctuation {
		t.Error("Describe claims punctuation the model does not produce")
	}
	if len(info.Languages) != 1 || info.Languages[0] != "ru" {
		t.Errorf("languages are %v", info.Languages)
	}
}

func TestPartialsAreThrottledToTheConfiguredInterval(t *testing.T) {
	rec := newFake()
	e := start(t, rec, Options{MaxSessions: 1, PartialInterval: time.Hour})

	s := open(t, e, core.RealtimeParams{AutoTurns: true})
	defer s.Close()

	// A lot of audio, so several words are decoded. With an hour between
	// partials only the first may be sent.
	speak(t, s, 16000, 0.6)

	deadline := time.After(300 * time.Millisecond)
	partials := 0
	for done := false; !done; {
		select {
		case ev := <-s.Events():
			if ev.Kind == core.RealtimePartial {
				partials++
			}
		case <-deadline:
			done = true
		}
	}
	if partials > 1 {
		t.Errorf("%d partials were sent although the interval is an hour", partials)
	}
}

func TestDeltaReportsAnExtensionAndNothingElse(t *testing.T) {
	cases := []struct{ previous, current, want string }{
		{"", "раз", "раз"},
		{"раз", "раз два", " два"},
		{"раз два", "раз два", ""},
		// A retraction: the recogniser changed its mind, so there is no delta
		// to report rather than a diff that would read as new speech.
		{"раз два", "раз три", ""},
		{"раз два", "раз", ""},
	}
	for _, c := range cases {
		if got := delta(c.previous, c.current); got != c.want {
			t.Errorf("delta(%q, %q) = %q, want %q", c.previous, c.current, got, c.want)
		}
	}
}

func codeOf(err error) core.Code { return core.AsError(err).Code }

// A client may upload faster than real time, so when it commits there can be
// several seconds still undecoded. The commit has to wait for them: finishing
// the utterance early truncates the transcript, which is exactly what the
// OpenAI SDK's own upload pattern produced before this was fixed.
func TestACommitWaitsForTheAudioThatPrecededIt(t *testing.T) {
	rec := newFake()
	// Six words, one per decoded chunk, and no endpoint of its own.
	rec.endpointAfter = 0
	e := start(t, rec, Options{MaxSessions: 1, MaxBufferedSeconds: 30})

	s := open(t, e, core.RealtimeParams{AutoTurns: true})
	defer s.Close()

	// 2.4 seconds in one burst: twenty-four chunks, three ticks' worth of
	// decoding at the round cap.
	for range 24 {
		if err := s.Append(16000, make([]float32, 1600)); err != nil {
			t.Fatal(err)
		}
	}
	s.Commit()

	evs := collect(t, s, core.RealtimeFinal)
	final, _ := find(evs, core.RealtimeFinal)
	// Every word the recogniser had to give, not the first eight chunks'
	// worth: the fake runs out of words before it runs out of audio.
	if final.Text != "раз два три четыре пять шесть" {
		t.Errorf("the committed transcript is %q, want the whole utterance", final.Text)
	}
}

// The session count is reported without asking the decoder loop, because the
// documentation page and every session acknowledgement read it.
func TestTheSessionCountIsVisibleWhileTheDecoderIsBusy(t *testing.T) {
	rec := newFake()
	rec.blockDecode = make(chan struct{})
	e := start(t, rec, Options{MaxSessions: 2})
	t.Cleanup(func() { close(rec.blockDecode) })

	if n := e.Active(); n != 0 {
		t.Errorf("Active() is %d on an idle engine", n)
	}

	s := open(t, e, core.RealtimeParams{})
	defer s.Close()
	if n := e.Active(); n != 1 {
		t.Errorf("Active() is %d with one session open", n)
	}

	// Wedge the loop inside a decode, then ask again: this must answer from
	// the counter rather than wait for the decode.
	if err := s.Append(16000, make([]float32, 16000)); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- e.Describe().Active }()
	select {
	case n := <-done:
		if n != 1 {
			t.Errorf("Describe().Active is %d, want 1", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Describe() blocked on the decoder loop")
	}
}
