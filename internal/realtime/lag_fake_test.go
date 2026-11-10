package realtime

import (
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/core"
)

// lagRecognizer is a streaming recogniser that behaves like a real one in the
// one respect the boundary bugs depend on: it writes a word some time after it
// has heard it.
//
// Every 100 ms of audio is one decode. A chunk of speech carries a word id in
// its amplitude, so the recogniser can say which word it heard, and the word is
// written lag decodes later. Silence advances the same clock, which is how the
// words still in flight come out when silence is fed. A Reset throws them
// away, as it does in a real stream: that is the defect.
//
// Its endpointer counts decodes that wrote nothing, as sherpa-onnx's does,
// which is what makes it fire while the beginning of the next sentence is
// still in flight.
type lagRecognizer struct {
	rate  int
	chunk int // samples per decode
	lag   int // decodes between a word's audio and the word
	// endpointNoText and endpointText are how many consecutive decodes that
	// wrote nothing end an utterance that has no words yet, and one that has.
	// Zero never does.
	endpointNoText int
	endpointText   int

	mu sync.Mutex
	// resetBacklog is, for each Reset, how many samples of audio the stream
	// still held undecoded.
	resetBacklog []int
	// leadRuns is, for each stream start and each Reset, how many samples of
	// silence the stream was given before the first sound.
	leadRuns []int
}

func newLag() *lagRecognizer {
	return &lagRecognizer{rate: 16000, chunk: 1600, lag: 5}
}

const (
	// wordFloor is the amplitude below which a chunk carries no word; a word's
	// id is added to it in thousandths. Noise sits well under it, and speech is
	// loud enough that the meter hears it as speech.
	wordFloor = 0.1
	noise     = 0.02
)

// word is one 100 ms chunk of speech that the lag recogniser hears as word id.
func word(id int) []float32 {
	amp := float32(wordFloor + float64(id)*0.001)
	out := make([]float32, 1600)
	for i := range out {
		if i%2 == 0 {
			out[i] = amp
		} else {
			out[i] = -amp
		}
	}
	return out
}

func hush(d float64) []float32 { return make([]float32, int(d*16000)) }

func lineNoise(d float64) []float32 {
	out := make([]float32, int(d*16000))
	for i := range out {
		if i%2 == 0 {
			out[i] = noise
		} else {
			out[i] = -noise
		}
	}
	return out
}

// speechIDs is speech: ids first..last, one per 100 ms.
func speechIDs(first, last int) []float32 {
	var out []float32
	for id := first; id <= last; id++ {
		out = append(out, word(id)...)
	}
	return out
}

// idOf is the word a chunk carries: the one at its middle. A stream that starts
// in the middle of a pause has its chunks offset from the words, and a chunk
// then holds the end of one word and the beginning of the next; the middle is
// the one it is mostly about, which is also how a real recogniser hears it.
func idOf(chunk []float32) int {
	if len(chunk) == 0 {
		return 0
	}
	m := chunk[len(chunk)/2]
	if m < 0 {
		m = -m
	}
	if m < wordFloor+0.0005 {
		return 0
	}
	return int(math.Round((float64(m) - wordFloor) * 1000))
}

func (r *lagRecognizer) NewStream() (asr.Stream, error) { return &lagStream{rec: r}, nil }

func (r *lagRecognizer) Decode(streams []asr.Stream) {
	for _, s := range streams {
		s.(*lagStream).decode()
	}
}

func (r *lagRecognizer) SampleRate() int      { return r.rate }
func (r *lagRecognizer) ModelID() string      { return "lag-streaming@1" }
func (r *lagRecognizer) ModelingUnit() string { return "bpe" }
func (r *lagRecognizer) Capabilities() core.Capabilities {
	return core.Capabilities{WordTimestamps: true}
}
func (r *lagRecognizer) Close() error { return nil }

// primings is how many times a stream was given silence before sound.
func (r *lagRecognizer) primings() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.leadRuns)
}

func (r *lagRecognizer) leads() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.leadRuns...)
}

func (r *lagRecognizer) backlogs() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.resetBacklog...)
}

type lagStream struct {
	rec     *lagRecognizer
	pending []float32
	// quiet counts the silent samples accepted since the stream started or was
	// reset, until the first sound; heard is whether there has been one.
	quiet int
	heard bool
	// inflight is the word id heard in each of the last decodes, oldest first.
	inflight []int
	decodes  int
	ids      []int
	stamps   []float32
	blanks   int
	finished bool
	closed   bool
}

func (s *lagStream) Accept(_ int, samples []float32) {
	if s.closed {
		return
	}
	s.pending = append(s.pending, samples...)
	for _, v := range samples {
		if s.heard {
			break
		}
		if v != 0 {
			s.heard = true
			s.rec.mu.Lock()
			s.rec.leadRuns = append(s.rec.leadRuns, s.quiet)
			s.rec.mu.Unlock()
			break
		}
		s.quiet++
	}
}

func (s *lagStream) SetLanguage(string) {}

func (s *lagStream) Ready() bool {
	if s.closed {
		return false
	}
	if s.finished {
		return len(s.pending) > 0
	}
	return len(s.pending) >= s.rec.chunk
}

func (s *lagStream) decode() {
	if s.closed || !s.Ready() {
		return
	}
	n := min(s.rec.chunk, len(s.pending))
	heard := idOf(s.pending[:n])
	s.pending = s.pending[n:]

	s.inflight = append(s.inflight, heard)
	wrote := false
	if len(s.inflight) > s.rec.lag {
		id := s.inflight[0]
		s.inflight = s.inflight[1:]
		if id != 0 {
			s.ids = append(s.ids, id)
			// The word's own position, not the decode that wrote it.
			s.stamps = append(s.stamps, float32(s.decodes-s.rec.lag)/10)
			wrote = true
		}
	}
	if wrote {
		s.blanks = 0
	} else {
		s.blanks++
	}
	s.decodes++
}

func (s *lagStream) Hypothesis() asr.Hypothesis {
	h := asr.Hypothesis{}
	var parts []string
	for i, id := range s.ids {
		tok := fmt.Sprintf("w%d", id)
		parts = append(parts, tok)
		h.Tokens = append(h.Tokens, "▁"+tok)
		h.Timestamps = append(h.Timestamps, s.stamps[i])
	}
	h.Text = strings.Join(parts, " ")
	return h
}

func (s *lagStream) Endpoint() bool {
	if s.closed {
		return false
	}
	if len(s.ids) == 0 {
		return s.rec.endpointNoText > 0 && s.blanks >= s.rec.endpointNoText
	}
	return s.rec.endpointText > 0 && s.blanks >= s.rec.endpointText
}

// Reset is the defect: whatever the model has heard and not yet written goes
// with it.
func (s *lagStream) Reset() {
	s.rec.mu.Lock()
	s.rec.resetBacklog = append(s.rec.resetBacklog, len(s.pending))
	s.rec.mu.Unlock()

	s.quiet, s.heard = 0, false
	s.inflight = nil
	s.decodes = 0
	s.ids = nil
	s.stamps = nil
	s.blanks = 0
}

func (s *lagStream) Finish() { s.finished = true }
func (s *lagStream) Close()  { s.closed = true }

var _ asr.Streaming = (*lagRecognizer)(nil)
var _ asr.Stream = (*lagStream)(nil)
