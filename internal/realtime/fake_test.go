package realtime

import (
	"sync"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/core"
)

// fakeRecognizer is a streaming recogniser with a script instead of weights.
//
// It behaves the way sherpa-onnx does in the two respects the engine depends
// on: a stream is ready once a chunk of audio has accumulated, and decoding
// consumes exactly one chunk. That makes the number of decode calls a function
// of the audio fed in, so a test can say "three hundred milliseconds of audio"
// and know what the engine will have done with it.
type fakeRecognizer struct {
	rate  int
	unit  string
	caps  core.Capabilities
	words []string
	// endpointAfter is how many decoded words end an utterance. 0 never does.
	endpointAfter int
	// chunkSamples is how much audio one decode consumes.
	chunkSamples int
	// failNewStream makes NewStream fail, as a loaded model that has been
	// closed underneath the engine would.
	failNewStream bool
	// blockDecode, when non-nil, holds every Decode until it is closed: a
	// decoder that has stopped, which is the only way a session's queue can
	// stay full.
	blockDecode chan struct{}

	mu      sync.Mutex
	decodes int
	streams int
	closed  bool
}

func newFake() *fakeRecognizer {
	return &fakeRecognizer{
		rate:         16000,
		unit:         "char",
		caps:         core.Capabilities{WordTimestamps: true},
		words:        []string{"раз", "два", "три", "четыре", "пять", "шесть"},
		chunkSamples: 1600, // 100 ms
	}
}

func (f *fakeRecognizer) NewStream() (asr.Stream, error) {
	if f.failNewStream {
		return nil, core.Errorf(core.CodeModelUnavailable, "no streams available")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.streams++
	return &fakeStream{rec: f}, nil
}

func (f *fakeRecognizer) Decode(streams []asr.Stream) {
	if f.blockDecode != nil {
		<-f.blockDecode
	}
	f.mu.Lock()
	f.decodes++
	f.mu.Unlock()
	for _, s := range streams {
		if fs, ok := s.(*fakeStream); ok {
			fs.decode()
		}
	}
}

func (f *fakeRecognizer) SampleRate() int                 { return f.rate }
func (f *fakeRecognizer) ModelID() string                 { return "fake-streaming@1" }
func (f *fakeRecognizer) ModelingUnit() string            { return f.unit }
func (f *fakeRecognizer) Capabilities() core.Capabilities { return f.caps }

func (f *fakeRecognizer) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeRecognizer) decodeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.decodes
}

// fakeStream is the per-session half. Every method is called from the engine's
// loop goroutine, so it needs no lock of its own — exactly like the real one.
type fakeStream struct {
	rec       *fakeRecognizer
	pending   int
	consumed  int
	spoken    int
	tokens    []string
	endpoint  bool
	finished  bool
	closed    bool
	resets    int
	acceptLog []int
}

func (s *fakeStream) Accept(sampleRate int, samples []float32) {
	if s.closed {
		return
	}
	s.pending += len(samples)
	s.acceptLog = append(s.acceptLog, sampleRate)
}

func (s *fakeStream) Ready() bool {
	if s.closed {
		return false
	}
	if s.finished {
		return s.pending > 0
	}
	return s.pending >= s.rec.chunkSamples
}

func (s *fakeStream) decode() {
	if s.closed {
		return
	}
	s.pending -= s.rec.chunkSamples
	if s.pending < 0 {
		s.pending = 0
	}
	s.consumed++
	if s.spoken < len(s.rec.words) {
		s.tokens = append(s.tokens, s.rec.words[s.spoken])
		s.spoken++
	}
	if n := s.rec.endpointAfter; n > 0 && len(s.tokens) >= n {
		s.endpoint = true
	}
}

func (s *fakeStream) Hypothesis() asr.Hypothesis {
	h := asr.Hypothesis{Text: joinWords(s.tokens)}
	for i := range s.tokens {
		// One token per 100 ms, which is what the chunk size implies.
		h.Tokens = append(h.Tokens, s.tokens[i])
		h.Timestamps = append(h.Timestamps, float32(i)/10)
	}
	return h
}

func (s *fakeStream) Endpoint() bool { return s.endpoint && !s.closed }

func (s *fakeStream) Reset() {
	s.resets++
	s.tokens = nil
	s.endpoint = false
}

func (s *fakeStream) Finish() { s.finished = true }
func (s *fakeStream) Close()  { s.closed = true }

func joinWords(w []string) string {
	out := ""
	for i, s := range w {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}

var _ asr.Streaming = (*fakeRecognizer)(nil)
var _ asr.Stream = (*fakeStream)(nil)
