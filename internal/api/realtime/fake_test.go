package realtime

import (
	"context"
	"sync"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

// fakeService stands in for the engine. The tests drive recognition events
// themselves, which is the only way to assert the wire protocol for a sequence
// a real model would reach only by chance.
type fakeService struct {
	info    core.RealtimeInfo
	openErr error

	mu       sync.Mutex
	sessions []*fakeSession
	opened   chan *fakeSession
}

func newFakeService() *fakeService {
	return &fakeService{
		info: core.RealtimeInfo{
			Model:          "t-one-ctc-ru@2025-09-08",
			Languages:      []string{"ru"},
			SampleRate:     8000,
			Punctuation:    false,
			WordTimestamps: true,
			AutoTurns:      true,
			MaxSessions:    4,
		},
		opened: make(chan *fakeSession, 4),
	}
}

func (f *fakeService) Open(_ context.Context, p core.RealtimeParams) (core.RealtimeSession, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	s := &fakeSession{
		id:     "sess_fake",
		params: p,
		events: make(chan core.RealtimeEvent, 16),
	}
	f.mu.Lock()
	f.sessions = append(f.sessions, s)
	f.mu.Unlock()
	f.opened <- s
	return s, nil
}

func (f *fakeService) Describe() core.RealtimeInfo { return f.info }

func (f *fakeService) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

type appended struct {
	rate    int
	samples []float32
}

type fakeSession struct {
	id     string
	params core.RealtimeParams
	events chan core.RealtimeEvent

	mu        sync.Mutex
	audio     []appended
	commits   int
	clears    int
	closes    int
	appendErr error
	// appendDelay stands in for a recogniser that has fallen behind, which is
	// what makes the real Append block.
	appendDelay time.Duration
}

func (s *fakeSession) Append(sampleRate int, samples []float32) error {
	s.mu.Lock()
	delay := s.appendDelay
	s.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.appendErr != nil {
		return s.appendErr
	}
	s.audio = append(s.audio, appended{rate: sampleRate, samples: samples})
	return nil
}

func (s *fakeSession) Commit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits++
}

func (s *fakeSession) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clears++
}

func (s *fakeSession) Events() <-chan core.RealtimeEvent { return s.events }
func (s *fakeSession) ID() string                        { return s.id }

func (s *fakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes++
	return nil
}

// emit pushes a recognition event towards the client.
func (s *fakeSession) emit(ev core.RealtimeEvent) { s.events <- ev }

// end closes the event channel, which is how a session reports that it is over.
func (s *fakeSession) end() { close(s.events) }

func (s *fakeSession) snapshot() (audio []appended, commits, clears, closes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]appended(nil), s.audio...), s.commits, s.clears, s.closes
}

// samples flattens every appended chunk, for assertions about what arrived.
func (s *fakeSession) samples() []float32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []float32
	for _, a := range s.audio {
		out = append(out, a.samples...)
	}
	return out
}

var _ core.RealtimeService = (*fakeService)(nil)
var _ core.RealtimeSession = (*fakeSession)(nil)
