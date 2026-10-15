package sherpa

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/core"
)

// scripted is a streaming recogniser that decodes by counting: a stream is
// ready once it holds a chunk of audio, and every decode round consumes one
// chunk and appends a token stamped with how much audio had been consumed.
type scripted struct {
	chunk int

	mu       sync.Mutex
	streams  []*scriptedStream
	inFlight atomic.Int32
	overlap  atomic.Bool
	rounds   atomic.Int32
	hook     func() // runs inside Decode, to cancel or to widen a race window
}

type scriptedStream struct {
	rec *scripted

	pending  int
	consumed int
	accepts  []int // lengths, in the order given
	language string
	finished bool
	closed   bool
	tokens   []string
	stamps   []float32
}

func (s *scripted) NewStream() (asr.Stream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &scriptedStream{rec: s}
	s.streams = append(s.streams, st)
	return st, nil
}

func (s *scripted) Decode(streams []asr.Stream) {
	if s.inFlight.Add(1) > 1 {
		s.overlap.Store(true)
	}
	defer s.inFlight.Add(-1)
	s.rounds.Add(1)
	if s.hook != nil {
		s.hook()
	}
	for _, x := range streams {
		st := x.(*scriptedStream)
		n := min(s.chunk, st.pending)
		st.pending -= n
		st.consumed += n
		st.tokens = append(st.tokens, "▁t")
		st.stamps = append(st.stamps, float32(st.consumed)/16000)
	}
}

func (s *scripted) SampleRate() int                 { return 16000 }
func (s *scripted) ModelID() string                 { return "scripted" }
func (s *scripted) ModelingUnit() string            { return "bpe" }
func (s *scripted) Capabilities() core.Capabilities { return core.Capabilities{WordTimestamps: true} }
func (s *scripted) Close() error                    { return nil }

func (st *scriptedStream) Accept(_ int, samples []float32) {
	st.accepts = append(st.accepts, len(samples))
	st.pending += len(samples)
}
func (st *scriptedStream) SetLanguage(l string) { st.language = l }
func (st *scriptedStream) Ready() bool {
	return st.pending >= st.rec.chunk || (st.finished && st.pending > 0)
}
func (st *scriptedStream) Hypothesis() asr.Hypothesis {
	return asr.Hypothesis{Text: "words", Tokens: st.tokens, Timestamps: st.stamps}
}
func (st *scriptedStream) Endpoint() bool { return false }
func (st *scriptedStream) Reset() {
	panic("a segment is one utterance: the stream must never be reset")
}
func (st *scriptedStream) Finish() { st.finished = true }
func (st *scriptedStream) Close()  { st.closed = true }

func seconds(n float64) []float32 { return make([]float32, int(n*16000)) }

func TestWholeFileDecodesEachSegmentAsOneUtterance(t *testing.T) {
	rec := &scripted{chunk: 8000}
	w := newWholeFile(rec)

	got, err := w.Decode(context.Background(), [][]float32{seconds(1), seconds(2.5)}, 16000, asr.DecodeOptions{Language: "de-DE"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || len(rec.streams) != 2 {
		t.Fatalf("%d results from %d streams, want 2 and 2", len(got), len(rec.streams))
	}

	for i, st := range rec.streams {
		if st.language != "de-DE" {
			t.Errorf("stream %d was told %q", i, st.language)
		}
		if !st.finished || !st.closed {
			t.Errorf("stream %d finished=%v closed=%v: the tail must be flushed and the stream released", i, st.finished, st.closed)
		}
		// Lead-in first, the segment, then the tail — in that order, or the
		// timestamps would no longer mean what the shift assumes.
		lead, tail := int(wholeFileLeadSeconds*16000), int(wholeFileTailSeconds*16000)
		if st.accepts[0] != lead || st.accepts[len(st.accepts)-1] != tail {
			t.Errorf("stream %d accepts = %v, want %d samples of silence first and %d last", i, st.accepts, lead, tail)
		}
		if st.pending != 0 {
			t.Errorf("stream %d left %d samples undecoded", i, st.pending)
		}
	}
	if got[0].Text != "words" || len(got[0].Tokens) == 0 {
		t.Errorf("result %+v", got[0])
	}
	if len(got[0].LogProbs) != 0 || len(got[0].Durations) != 0 {
		t.Error("a streaming result has neither log-probabilities nor durations, and must not invent them")
	}
}

// The lead-in is the model's business and not the caller's: a timestamp is
// measured from the start of the segment, as asr.Recognition documents.
func TestWholeFileTimestampsAreRelativeToTheSegment(t *testing.T) {
	rec := &scripted{chunk: 8000}
	got, err := newWholeFile(rec).Decode(context.Background(), [][]float32{seconds(1)}, 16000, asr.DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st := rec.streams[0]
	for i, ts := range got[0].Timestamps {
		want := st.stamps[i] - 0.5
		if want < 0 {
			want = 0
		}
		if ts != want {
			t.Errorf("timestamp %d = %v, want %v (stream's %v less the 0.5 s lead-in)", i, ts, want, st.stamps[i])
		}
		if ts < 0 {
			t.Errorf("timestamp %d is negative: %v", i, ts)
		}
	}
	if got[0].Timestamps[0] != 0 {
		t.Errorf("a token inside the lead-in must land on zero, got %v", got[0].Timestamps[0])
	}
}

func TestWholeFileFeedsLongSegmentsInSlices(t *testing.T) {
	rec := &scripted{chunk: 8000}
	if _, err := newWholeFile(rec).Decode(context.Background(), [][]float32{seconds(25)}, 16000, asr.DecodeOptions{}); err != nil {
		t.Fatal(err)
	}
	// The lead-in, 25 s as 10+10+5, then the tail: five accepts, and none of
	// the segment's slices is longer than the feed size.
	if got := rec.streams[0].accepts; len(got) != 5 || got[1] != 160000 || got[2] != 160000 || got[3] != 80000 {
		t.Errorf("accepts = %v", got)
	}
}

func TestWholeFileEmptyBatch(t *testing.T) {
	rec := &scripted{chunk: 8000}
	got, err := newWholeFile(rec).Decode(context.Background(), nil, 16000, asr.DecodeOptions{})
	if err != nil || len(got) != 0 || len(rec.streams) != 0 {
		t.Errorf("got %v, %v, %d streams", got, err, len(rec.streams))
	}
}

func TestWholeFileStopsWhenCancelledAndStillReleasesItsStreams(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &scripted{chunk: 8000, hook: cancel}

	_, err := newWholeFile(rec).Decode(ctx, [][]float32{seconds(30)}, 16000, asr.DecodeOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !rec.streams[0].closed {
		t.Error("a cancelled decode leaked its stream")
	}
	// Cancelled during the first round: it must not run to the end of 30 s.
	if r := rec.rounds.Load(); r > 2 {
		t.Errorf("%d rounds after cancellation", r)
	}
}

// The recogniser is shared by every worker holding a lease, and the streaming
// one reuses scratch space that assumes a single caller.
func TestWholeFileSerialisesDecoderRounds(t *testing.T) {
	rec := &scripted{chunk: 8000, hook: func() {
		for i := 0; i < 200; i++ { // widen the window an overlap would need
			_ = i
		}
	}}
	w := newWholeFile(rec)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := w.Decode(context.Background(), [][]float32{seconds(3)}, 16000, asr.DecodeOptions{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if rec.overlap.Load() {
		t.Error("two decoder rounds ran at once")
	}
}
