package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamSetsTheHeadersThatKeepItLive(t *testing.T) {
	rec := httptest.NewRecorder()
	s, err := NewStream(rec)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	defer s.Close()

	want := map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"X-Accel-Buffering": "no",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestSendFramesAnEvent(t *testing.T) {
	rec := httptest.NewRecorder()
	s, _ := NewStream(rec)
	defer s.Close()

	if err := s.Send("7", "status", map[string]string{"id": "job_1"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := rec.Body.String()
	for _, want := range []string{"id: 7\n", "event: status\n", `data: {"id":"job_1"}` + "\n", "\n\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("body %q does not contain %q", got, want)
		}
	}
}

func TestSendOmitsTheOptionalLines(t *testing.T) {
	rec := httptest.NewRecorder()
	s, _ := NewStream(rec)
	defer s.Close()

	if err := s.Send("", "", "ok"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := rec.Body.String(); got != "data: \"ok\"\n\n" {
		t.Errorf("body = %q, want no id or event line when both are empty", got)
	}
}

// A blank line terminates an event, so a payload carrying a newline has to be
// written one data field per line or the client sees a truncated document.
// encoding/json escapes newlines today, which is why this tests the framing
// directly rather than through Send.
func TestFrameSplitsAMultilinePayload(t *testing.T) {
	got := frame("3", "status", []byte("first\nsecond"))
	want := "id: 3\nevent: status\ndata: first\ndata: second\n\n"
	if got != want {
		t.Errorf("frame = %q, want %q", got, want)
	}
}

func TestCommentIsIgnorableTraffic(t *testing.T) {
	rec := httptest.NewRecorder()
	s, _ := NewStream(rec)
	defer s.Close()

	if err := s.Comment("keep-alive"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if got := rec.Body.String(); got != ": keep-alive\n\n" {
		t.Errorf("body = %q", got)
	}
}

func TestNewStreamRefusesAWriterThatCannotFlush(t *testing.T) {
	if _, err := NewStream(unflushable{httptest.NewRecorder()}); err == nil {
		t.Fatal("NewStream accepted a writer that cannot stream")
	}
}

type unflushable struct{ http.ResponseWriter }

func TestLastEventIDReadsHeaderOrQuery(t *testing.T) {
	cases := []struct {
		name   string
		header string
		query  string
		want   int64
	}{
		{"header", "12", "", 12},
		{"query fallback", "", "?last_event_id=5", 5},
		{"header wins", "12", "?last_event_id=5", 12},
		{"absent", "", "", 0},
		{"garbage", "abc", "", 0},
		{"negative", "-3", "", 0},
		{"padded", " 8 ", "", 8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/x/events"+c.query, nil)
			if c.header != "" {
				r.Header.Set("Last-Event-ID", c.header)
			}
			if got := LastEventID(r); got != c.want {
				t.Errorf("LastEventID = %d, want %d", got, c.want)
			}
		})
	}
}
