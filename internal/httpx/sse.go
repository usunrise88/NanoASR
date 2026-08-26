package httpx

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// heartbeat is how often an idle stream emits a comment line.
//
// Without it a reverse proxy closes a connection that has been silent for its
// idle timeout, and a job that spends four minutes in the ASR stage is silent
// for four minutes. A comment is the cheapest thing that is still traffic: the
// EventSource protocol ignores lines starting with a colon.
const heartbeat = 15 * time.Second

// writeDeadline caps each individual write to the response. It is set per
// write rather than on http.Server: a slow client that stops reading would
// otherwise pin a goroutine and a job-hub subscription forever, because the
// request context does not interrupt a blocked HTTP/1.1 write.
const writeDeadline = 30 * time.Second

// Stream writes server-sent events.
//
// Two endpoints need this — job transitions and model download progress — and
// both need the same unglamorous details right: the flush after every event,
// the headers that stop intermediaries from buffering, and a heartbeat.
type Stream struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	ticker   *time.Ticker
	deadline time.Duration
}

// NewStream sets the headers and returns a writer, or an error if the response
// cannot be streamed at all.
func NewStream(w http.ResponseWriter) (*Stream, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("sse: the response writer cannot flush")
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// nginx buffers proxied responses by default, which turns a live stream
	// into one delivery at the end. This is how it is told not to.
	h.Set("X-Accel-Buffering", "no")

	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	return &Stream{w: w, flusher: flusher, ticker: time.NewTicker(heartbeat),
		deadline: writeDeadline}, nil
}

// Send writes one event. id may be empty; name defaults to "message".
func (s *Stream) Send(id, name string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("sse: encode %s: %w", name, err)
	}

	if err := s.write([]byte(frame(id, name, body))); err != nil {
		return err
	}
	s.flusher.Flush()
	s.resetHeartbeat()
	return nil
}

// frame renders one event.
//
// The data field is written a line at a time because a blank line terminates an
// event: a payload containing a newline written raw would be truncated at the
// client. encoding/json escapes newlines, so today every payload is one line —
// this is what keeps that from being a load-bearing accident.
func frame(id, name string, body []byte) string {
	var b strings.Builder
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	if name != "" {
		b.WriteString("event: " + name + "\n")
	}
	for _, line := range strings.Split(string(body), "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

// Comment writes a no-op line. Used as the heartbeat.
func (s *Stream) Comment(text string) error {
	if err := s.write([]byte(": " + text + "\n\n")); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// write sets a per-call deadline and then writes. A client that stops reading
// cannot pin the goroutine past this deadline: a blocked HTTP/1.1 write does
// not observe request context cancellation, but SetWriteDeadline does.
func (s *Stream) write(p []byte) error {
	rc := http.NewResponseController(s.w)
	if err := rc.SetWriteDeadline(time.Now().Add(s.deadline)); err != nil {
		// The writer did not support deadlines; fall through and let the
		// kernel buffer fill — the same as before this safety net existed.
		_, werr := s.w.Write(p)
		return werr
	}
	if _, err := s.w.Write(p); err != nil {
		return err
	}
	return nil
}

// Heartbeat fires when the stream has been silent long enough to worry a proxy.
func (s *Stream) Heartbeat() <-chan time.Time { return s.ticker.C }

func (s *Stream) resetHeartbeat() { s.ticker.Reset(heartbeat) }

func (s *Stream) Close() { s.ticker.Stop() }

// LastEventID is the sequence number a reconnecting client already has.
//
// The header is what EventSource sends on its own; the query parameter is for
// clients that read the stream with fetch instead, which is how the test UI has
// to do it — EventSource cannot set an Authorization header, and putting a token
// in the query string would write it straight into the access log.
func LastEventID(r *http.Request) int64 {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("last_event_id")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
