package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

func TestATicketOpensOnePathForReading(t *testing.T) {
	store := newStore(t, KeySpec{Name: "app", Secret: goodKey}, KeySpec{Name: "ci", Secret: adminKey, Admin: true})
	tickets := NewTickets(time.Hour)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	tickets.now = func() time.Time { return now }

	mux := http.NewServeMux()
	for _, p := range []string{"/v1/audio", "/v1/other"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(APIKeyID(r.Context())))
		})
	}
	h := Chain(mux, Auth(store, tickets))
	do := func(method, target string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, target, nil))
		return w
	}

	app, _ := store.Verify(t.Context(), goodKey)
	signed, exp := tickets.Sign(core.Caller{KeyID: app}, "/v1/audio")
	if !exp.Equal(now.Add(time.Hour)) {
		t.Errorf("expires %v", exp)
	}
	if w := do(http.MethodGet, signed); w.Code != http.StatusOK || w.Body.String() != app {
		t.Fatalf("the signed path: %d %q", w.Code, w.Body.String())
	}
	// Other query parameters ride along: the path is what was signed.
	if w := do(http.MethodGet, signed+"&format=wav"); w.Code != http.StatusOK {
		t.Errorf("with a format: %d", w.Code)
	}
	if w := do(http.MethodHead, signed); w.Code != http.StatusOK {
		t.Errorf("HEAD: %d", w.Code)
	}

	ticket := signed[strings.Index(signed, "?"):]
	for name, target := range map[string]string{
		"another path":   "/v1/other" + ticket,
		"no ticket":      "/v1/audio",
		"a forged one":   "/v1/audio" + strings.Replace(ticket, ".", ".x", 1),
		"a cut-off one":  signed[:len(signed)-3],
		"a ticket alone": "/v1/audio?ticket=" + strings.Repeat("a", 20),
	} {
		if w := do(http.MethodGet, target); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if w := do(http.MethodPost, signed); w.Code != http.StatusUnauthorized {
		t.Errorf("a write with a ticket: %d", w.Code)
	}

	// Expired.
	now = now.Add(time.Hour + time.Second)
	if w := do(http.MethodGet, signed); w.Code != http.StatusUnauthorized {
		t.Errorf("expired: %d", w.Code)
	}

	// A key that is gone opens nothing, whatever it was given.
	now = now.Add(-time.Hour)
	ghost, _ := tickets.Sign(core.Caller{KeyID: "gone"}, "/v1/audio")
	if w := do(http.MethodGet, ghost); w.Code != http.StatusUnauthorized {
		t.Errorf("an unknown key: %d", w.Code)
	}
	// A server without tickets admits none.
	plain := Chain(mux, Auth(store, nil))
	w := httptest.NewRecorder()
	plain.ServeHTTP(w, httptest.NewRequest(http.MethodGet, signed, nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("without tickets: %d", w.Code)
	}
}
