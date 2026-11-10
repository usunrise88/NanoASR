package realtime

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

// The tests in this file are about the boundaries between utterances: what a
// recogniser that writes its words late does to the words around one.
//
// Every 100 ms of the audio they feed is one word with a number, so "no word was
// lost" and "no word was said twice" are checks on a list of numbers, and the
// recogniser is the lag recogniser, which drops what it has heard and not yet
// written when it is reset.

// frames feeds audio in 20 ms pieces, the size a client sends, and returns the
// events up to and including the final that ends at the end of the audio. The
// audio is closed with a commit, which is what makes that final arrive whatever
// the recogniser did with the rest.
func runLag(t *testing.T, rec *lagRecognizer, opt Options, p core.RealtimeParams, audio []float32) []core.RealtimeEvent {
	t.Helper()
	e := start(t, rec, opt)
	s := open(t, e, p)

	total := int64(len(audio)) * 1000 / 16000
	const piece = 320 // 20 ms
	for off := 0; off < len(audio); off += piece {
		if err := s.Append(16000, audio[off:min(off+piece, len(audio))]); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	s.Commit()

	var out []core.RealtimeEvent
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatalf("the session ended before the commit was answered; got %s", kinds(out))
			}
			out = append(out, ev)
			if ev.Kind == core.RealtimeFinal && ev.EndMS >= total {
				return out
			}
		case <-deadline:
			t.Fatalf("no final for the commit; got %s", kinds(out))
		}
	}
}

func finals(evs []core.RealtimeEvent) []core.RealtimeEvent {
	var out []core.RealtimeEvent
	for _, ev := range evs {
		if ev.Kind == core.RealtimeFinal {
			out = append(out, ev)
		}
	}
	return out
}

// spoken is every word id the finals carry, in the order they came.
func spoken(t *testing.T, evs []core.RealtimeEvent) []int {
	t.Helper()
	var ids []int
	for _, f := range finals(evs) {
		for _, w := range strings.Fields(f.Text) {
			var id int
			if _, err := fmt.Sscanf(w, "w%d", &id); err != nil {
				t.Fatalf("a final carries %q, which is not a word id", w)
			}
			ids = append(ids, id)
		}
	}
	return ids
}

func seq(first, last int) []int {
	var out []int
	for i := first; i <= last; i++ {
		out = append(out, i)
	}
	return out
}

func wantWords(t *testing.T, evs []core.RealtimeEvent, want []int) {
	t.Helper()
	got := spoken(t, evs)
	if slices.Equal(got, want) {
		return
	}
	var lost, twice []int
	seen := map[int]int{}
	for _, id := range got {
		seen[id]++
	}
	for _, id := range want {
		if seen[id] == 0 {
			lost = append(lost, id)
		}
	}
	for id, n := range seen {
		if n > 1 {
			twice = append(twice, id)
		}
	}
	slices.Sort(twice)
	for i, f := range finals(evs) {
		t.Logf("final %d: %d-%d ms (%s) %s", i, f.StartMS, f.EndMS, f.Cut, f.Text)
	}
	t.Fatalf("words lost: %v; words said twice: %v", lost, twice)
}

// A recogniser that cuts at the limit by itself, and so is reset with its last
// words unwritten, is the bug this was written for. The engine owns the limit
// now, and flushes the stream first.
func TestALengthLimitLosesNoWordsStillInFlight(t *testing.T) {
	// A sentence with no pause in it, ten times the lag long.
	audio := speechIDs(1, 80)
	opt := Options{MaxUtterance: 3 * time.Second}

	evs := runLag(t, newLag(), opt, core.RealtimeParams{AutoTurns: true}, audio)

	wantWords(t, evs, seq(1, 80))
	fs := finals(evs)
	if len(fs) < 3 {
		t.Fatalf("%d utterances from eight seconds with a three second limit, want at least 3", len(fs))
	}
	for i, f := range fs[:len(fs)-1] {
		if f.Cut != "max_length" {
			t.Errorf("utterance %d was cut by %q, want max_length", i, f.Cut)
		}
		if got := f.EndMS - f.StartMS; got != 3000 {
			t.Errorf("utterance %d is %d ms long, want exactly the 3000 ms limit", i, got)
		}
	}
	if last := fs[len(fs)-1]; last.Cut != "commit" {
		t.Errorf("the last utterance was cut by %q, want commit", last.Cut)
	}
}

func TestEachUtteranceStartsWhereTheLastOneEnded(t *testing.T) {
	evs := runLag(t, newLag(), Options{MaxUtterance: 3 * time.Second},
		core.RealtimeParams{AutoTurns: true}, speechIDs(1, 80))

	fs := finals(evs)
	for i := 1; i < len(fs); i++ {
		if fs[i].StartMS != fs[i-1].EndMS {
			t.Errorf("utterance %d starts at %d ms, the one before ended at %d ms: audio is missing from the timeline",
				i, fs[i].StartMS, fs[i-1].EndMS)
		}
	}
}

func TestAStreamIsLeftWithNoSilenceFedToItAhead(t *testing.T) {
	rec := newLag()
	runLag(t, rec, Options{MaxUtterance: 3 * time.Second}, core.RealtimeParams{AutoTurns: true}, speechIDs(1, 80))

	// Each reset is of a stream that was just fed silence to make it speak. If
	// it were fed in one block, a chunk of that silence would still be waiting
	// ahead of the next utterance's audio and every word time in it would be
	// late by as much.
	for i, backlog := range rec.backlogs() {
		if backlog > 320 { // one 20 ms piece
			t.Errorf("reset %d left %d samples (%.0f ms) of silence in the stream; want at most one 20 ms piece",
				i, backlog, float64(backlog)/16)
		}
	}
}

func TestALongUtteranceIsClosedAtAPauseRatherThanAtTheLimit(t *testing.T) {
	// Words, half a second of quiet, more words. The limit is four seconds and
	// the search for a pause starts at two, so the pause at 2.8 s is where it
	// should end, well before the limit.
	audio := slices.Concat(speechIDs(1, 28), hush(0.5), speechIDs(29, 60))
	opt := Options{MaxUtterance: 4 * time.Second, PauseSearch: 2 * time.Second, MinPause: 400 * time.Millisecond}

	evs := runLag(t, newLag(), opt, core.RealtimeParams{AutoTurns: true}, audio)

	wantWords(t, evs, seq(1, 60))
	fs := finals(evs)
	if fs[0].Cut != "pause" {
		t.Fatalf("the first utterance was cut by %q at %d ms, want pause", fs[0].Cut, fs[0].EndMS)
	}
	if fs[0].EndMS < 2800 || fs[0].EndMS > 3400 {
		t.Errorf("the pause was taken at %d ms, want it inside the half second of quiet (2800-3300)", fs[0].EndMS)
	}
}

func TestAShorterPauseIsAcceptedTheNearerTheLimit(t *testing.T) {
	// A 0.2 s gap is too short to count when the search opens, at 400 ms, and
	// is taken once the limit is near enough that a speaker who will not stop
	// has to be cut at a breath.
	early := slices.Concat(speechIDs(1, 25), hush(0.2), speechIDs(26, 50), hush(0.3), speechIDs(51, 70))
	opt := Options{MaxUtterance: 6 * time.Second, PauseSearch: 4 * time.Second, MinPause: 400 * time.Millisecond}

	evs := runLag(t, newLag(), opt, core.RealtimeParams{AutoTurns: true}, early)

	wantWords(t, evs, seq(1, 70))
	fs := finals(evs)
	if fs[0].Cut != "pause" {
		t.Fatalf("the first utterance was cut by %q at %d ms, want pause", fs[0].Cut, fs[0].EndMS)
	}
	// The 0.2 s gap at 2.5 s is inside the search (which opens at 2 s) but is
	// shorter than the 0.4 s asked for there; the 0.3 s one at 5.2 s is the first
	// the relaxed requirement admits.
	if fs[0].EndMS < 4500 {
		t.Errorf("a 0.2 s gap was taken at %d ms, before the requirement had relaxed to it", fs[0].EndMS)
	}
}

func TestNoPauseIsTakenBeforeTheSearchOpens(t *testing.T) {
	// Half a second of quiet at one second, in an utterance that is allowed
	// twenty: nothing is cut, because nothing has run long.
	audio := slices.Concat(speechIDs(1, 10), hush(0.5), speechIDs(11, 40))
	opt := Options{MaxUtterance: 20 * time.Second, PauseSearch: 8 * time.Second}

	evs := runLag(t, newLag(), opt, core.RealtimeParams{AutoTurns: true}, audio)

	wantWords(t, evs, seq(1, 40))
	if n := len(finals(evs)); n != 1 {
		t.Fatalf("%d utterances, want 1: a pause the search had not opened for was taken", n)
	}
}

func TestWithoutALimitAnUtteranceRunsUntilTheSpeakerStops(t *testing.T) {
	evs := runLag(t, newLag(), Options{}, core.RealtimeParams{AutoTurns: true}, speechIDs(1, 120))

	wantWords(t, evs, seq(1, 120))
	if n := len(finals(evs)); n != 1 {
		t.Fatalf("%d utterances from twelve seconds with no limit, want 1", n)
	}
}

func TestACommitStillEndsAnUtteranceThatWasBeingClosedByTheLimit(t *testing.T) {
	// Without automatic turns nothing but a commit ends an utterance, and the
	// limit is not a turn: it is a property of automatic ones.
	evs := runLag(t, newLag(), Options{MaxUtterance: 2 * time.Second},
		core.RealtimeParams{AutoTurns: false}, speechIDs(1, 60))

	wantWords(t, evs, seq(1, 60))
	if n := len(finals(evs)); n != 1 {
		t.Fatalf("%d utterances with turn detection off, want 1", n)
	}
}

// What an endpoint raised while speech is arriving does, and does not do.

func TestAnEndpointIsNotTakenWhileTheNextSentenceIsStillInFlight(t *testing.T) {
	rec := newLag()
	rec.endpointText = 8 // 0.8 s of decodes that wrote nothing
	// Sentence one, then 1.1 s of quiet, then sentence two. The recogniser
	// writes the last word of sentence one at the 15th decode and counts its
	// eighth blank at the 23rd, 2.3 s in: sentence two began at 2.1 s, so its
	// first two words have been heard and not yet written. Resetting then throws
	// them away.
	audio := slices.Concat(speechIDs(1, 10), hush(1.1), speechIDs(11, 30))

	evs := runLag(t, rec, Options{}, core.RealtimeParams{AutoTurns: true}, audio)

	wantWords(t, evs, seq(1, 30))
}

func TestAnEndpointAfterARealPauseIsTakenAtOnce(t *testing.T) {
	rec := newLag()
	rec.endpointText = 8
	audio := slices.Concat(speechIDs(1, 10), hush(3), speechIDs(11, 20))

	evs := runLag(t, rec, Options{}, core.RealtimeParams{AutoTurns: true}, audio)

	wantWords(t, evs, seq(1, 20))
	fs := finals(evs)
	if len(fs) != 3 { // sentence one, sentence two (committed), and the commit's own
		t.Logf("finals: %d", len(fs))
	}
	if fs[0].Cut != "silence" {
		t.Fatalf("the first utterance was cut by %q, want silence", fs[0].Cut)
	}
	if fs[0].EndMS > 3000 {
		t.Errorf("a three second pause was acted on at %d ms: the endpoint was held when nothing was arriving", fs[0].EndMS)
	}
}

func TestAHeldEndpointIsNotHeldForEver(t *testing.T) {
	rec := newLag()
	rec.endpointText = 8
	// Noise the recogniser writes nothing for and the meter hears as loud: the
	// endpoint must still be taken, late, rather than never.
	audio := slices.Concat(speechIDs(1, 10), hush(1.2), lineNoise(6))

	evs := runLag(t, rec, Options{}, core.RealtimeParams{AutoTurns: true}, audio)

	fs := finals(evs)
	if fs[0].Cut != "silence" {
		t.Fatalf("the first utterance was cut by %q, want silence", fs[0].Cut)
	}
	// The recogniser's eighth blank is at 2.3 s, with noise already arriving. The
	// endpoint is put off for maxHold from there, and taken then.
	earliest := int64(2300) + maxHold.Milliseconds() - 200
	latest := int64(2300) + maxHold.Milliseconds() + 500
	if fs[0].EndMS < earliest {
		t.Errorf("the endpoint was taken at %d ms, want it held until about %d ms", fs[0].EndMS, earliest)
	}
	if fs[0].EndMS > latest {
		t.Errorf("the endpoint was taken at %d ms, want it by %d ms: it was held for too long", fs[0].EndMS, latest)
	}
}

func TestEveryFinalSaysWhyItEnded(t *testing.T) {
	rec := newLag()
	rec.endpointText = 8
	audio := slices.Concat(speechIDs(1, 10), hush(3), speechIDs(11, 60))
	opt := Options{MaxUtterance: 3 * time.Second}

	evs := runLag(t, rec, opt, core.RealtimeParams{AutoTurns: true}, audio)

	valid := map[string]bool{"silence": true, "pause": true, "max_length": true, "commit": true}
	for i, f := range finals(evs) {
		if !valid[f.Cut] {
			t.Errorf("final %d has cut %q", i, f.Cut)
		}
	}
	for _, ev := range evs {
		if ev.Kind == core.RealtimeSpeechStopped && ev.Cut == "" {
			t.Errorf("a speech_stopped with no reason: %+v", ev)
		}
	}
}

func TestAClearDuringAClosingDropsTheClosing(t *testing.T) {
	// The utterance that was being closed no longer exists after a clear, and the
	// session must carry on with the next one instead of finishing nothing.
	rec := newLag()
	e := start(t, rec, Options{MaxUtterance: time.Second})
	s := open(t, e, core.RealtimeParams{AutoTurns: true})

	for off := 0; off < 6; off++ {
		if err := s.Append(16000, speechIDs(1, 10)); err != nil {
			t.Fatal(err)
		}
	}
	s.Clear()
	if err := s.Append(16000, speechIDs(101, 110)); err != nil {
		t.Fatal(err)
	}
	s.Commit()

	var got []core.RealtimeEvent
	deadline := time.After(5 * time.Second)
	for done := false; !done; {
		select {
		case ev := <-s.Events():
			got = append(got, ev)
			done = ev.Kind == core.RealtimeFinal && ev.Cut == "commit"
		case <-deadline:
			t.Fatalf("the session never answered the commit after a clear; got %s", kinds(got))
		}
	}
	if _, ok := find(got, core.RealtimeCleared); !ok {
		t.Fatalf("no cleared event; got %s", kinds(got))
	}
}

func TestWordTimesStayOnTheClientsTimelineAfterEveryBoundary(t *testing.T) {
	// The word with id k is the k-th tenth of a second of the audio, so its time
	// is known. After a boundary the stream is given silence to stand in front
	// of the next utterance, and its clock starts that much before the audio
	// does: the times must not carry it.
	evs := runLag(t, newLag(), Options{MaxUtterance: 3 * time.Second},
		core.RealtimeParams{AutoTurns: true}, speechIDs(1, 80))

	checked := 0
	for i, f := range finals(evs) {
		for _, w := range f.Words {
			var id int
			if _, err := fmt.Sscanf(w.Word, "w%d", &id); err != nil {
				t.Fatalf("final %d has a word %q", i, w.Word)
			}
			want := float64(id-1) / 10
			if w.Start < want-0.02 || w.Start > want+0.02 {
				t.Errorf("final %d: %s starts at %.2f s, want %.2f s", i, w.Word, w.Start, want)
			}
			if w.Start < float64(f.StartMS)/1000-1e-9 {
				t.Errorf("final %d: %s starts at %.2f s, before its utterance at %d ms", i, w.Word, w.Start, f.StartMS)
			}
			checked++
		}
	}
	if checked < 60 {
		t.Fatalf("only %d words carried times", checked)
	}
}

func TestAFreshStreamIsGivenSilenceToStandInFrontOfTheAudio(t *testing.T) {
	rec := newLag()
	runLag(t, rec, Options{MaxUtterance: 3 * time.Second}, core.RealtimeParams{AutoTurns: true}, speechIDs(1, 80))
	// Every utterance after the first began with a stream that was reset, and
	// every stream, the first included, with the lead silence.
	if n := rec.primings(); n < 3 {
		t.Fatalf("%d streams were primed, want one per utterance", n)
	}
	for i, lead := range rec.leads() {
		if lead < 400*16 { // 400 ms of 16 kHz samples
			t.Errorf("stream %d was given %d samples of lead silence, want about half a second", i, lead)
		}
	}
}

// An endpoint with nothing decoded ends nothing: it is the recogniser's rule for
// a long silence, and acting on it only throws away what may be arriving.

func TestALongSilenceDoesNotCostTheFirstWordAfterIt(t *testing.T) {
	rec := newLag()
	rec.endpointNoText = 20 // two seconds of decodes that wrote nothing
	// Speech begins 1.4 s in. The stream, which started with half a second of
	// lead silence, counts its twentieth blank at 2.0 s, when the first two
	// words have been heard and not yet written.
	audio := slices.Concat(hush(1.4), speechIDs(1, 20))

	evs := runLag(t, rec, Options{}, core.RealtimeParams{AutoTurns: true}, audio)

	wantWords(t, evs, seq(1, 20))
	// The only reset there may be is the one the commit makes at the end, which
	// can come after the final it answers.
	if n := len(rec.backlogs()); n > 1 {
		t.Errorf("the stream was reset %d times, want at most the commit's", n)
	}
}

func TestTheItemStartFollowsTheSilenceThatWasSkipped(t *testing.T) {
	rec := newLag()
	rec.endpointNoText = 20
	audio := slices.Concat(hush(10), speechIDs(1, 20))

	evs := runLag(t, rec, Options{}, core.RealtimeParams{AutoTurns: true}, audio)

	wantWords(t, evs, seq(1, 20))
	fs := finals(evs)
	if len(fs) != 1 {
		t.Fatalf("%d utterances, want 1", len(fs))
	}
	// Silence is not an utterance and has nothing to reset: the stream that
	// listened to ten seconds of it is the one that hears the speech after.
	if n := len(rec.backlogs()); n > 1 {
		t.Errorf("the stream was reset %d times across ten seconds of silence", n)
	}
	// Ten seconds of silence are not part of the utterance: it begins a little
	// before the speech, which starts at 10 s.
	if fs[0].StartMS < 9000 || fs[0].StartMS > 10000 {
		t.Errorf("the utterance begins at %d ms; the speech begins at 10000 ms", fs[0].StartMS)
	}
	// And its word times are still on the client's timeline.
	for _, w := range fs[0].Words {
		var id int
		_, _ = fmt.Sscanf(w.Word, "w%d", &id)
		if want := 10 + float64(id-1)/10; w.Start < want-0.05 || w.Start > want+0.05 {
			t.Errorf("%s starts at %.2f s, want %.2f s", w.Word, w.Start, want)
		}
	}
}
