package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

func limiter(rates map[string]float64) *RateLimiter {
	return NewRateLimiter(func(id string) (float64, bool) {
		rate, ok := rates[id]
		return rate, ok
	})
}

// A clock a test controls, so refill behaviour does not depend on sleeping.
func atTime(l *RateLimiter, t *time.Time) { l.now = func() time.Time { return *t } }

func TestAllowSpendsTheBurstThenRefuses(t *testing.T) {
	now := time.Now()
	l := limiter(map[string]float64{"key-a": 2})
	atTime(l, &now)

	// burstFactor seconds' worth is available immediately.
	for i := range 4 {
		if ok, _ := l.Allow("key-a"); !ok {
			t.Fatalf("request %d was refused inside the burst", i+1)
		}
	}
	ok, wait := l.Allow("key-a")
	if ok {
		t.Fatal("the burst never ran out")
	}
	if wait <= 0 {
		t.Errorf("wait = %v, want a positive delay", wait)
	}
}

func TestTokensRefillOverTime(t *testing.T) {
	now := time.Now()
	l := limiter(map[string]float64{"key-a": 2})
	atTime(l, &now)

	for range 4 {
		l.Allow("key-a")
	}
	if ok, _ := l.Allow("key-a"); ok {
		t.Fatal("expected the bucket to be empty")
	}

	now = now.Add(time.Second) // two tokens at 2 rps
	for i := range 2 {
		if ok, _ := l.Allow("key-a"); !ok {
			t.Fatalf("refilled request %d was refused", i+1)
		}
	}
	if ok, _ := l.Allow("key-a"); ok {
		t.Error("refill exceeded the elapsed time")
	}
}

func TestRefillIsCappedAtTheBurst(t *testing.T) {
	now := time.Now()
	l := limiter(map[string]float64{"key-a": 1})
	atTime(l, &now)

	now = now.Add(time.Hour) // an idle key must not bank an hour of requests
	allowed := 0
	for range 10 {
		if ok, _ := l.Allow("key-a"); ok {
			allowed++
		}
	}
	if allowed != burstFactor {
		t.Errorf("allowed %d after an idle hour, want %d", allowed, burstFactor)
	}
}

func TestKeysAreLimitedIndependently(t *testing.T) {
	now := time.Now()
	l := limiter(map[string]float64{"key-a": 1, "key-b": 1})
	atTime(l, &now)

	for range 2 {
		l.Allow("key-a")
	}
	if ok, _ := l.Allow("key-a"); ok {
		t.Fatal("key-a still had allowance")
	}
	if ok, _ := l.Allow("key-b"); !ok {
		t.Error("key-b was throttled by key-a's traffic")
	}
}

func TestAnUnlimitedKeyIsNeverRefused(t *testing.T) {
	l := limiter(map[string]float64{"key-a": 0})
	for i := range 100 {
		if ok, _ := l.Allow("key-a"); !ok {
			t.Fatalf("request %d was refused for a key with no limit", i)
		}
	}
}

func TestAnUnknownKeyIsNotThrottled(t *testing.T) {
	l := limiter(nil)
	if ok, _ := l.Allow("key-missing"); !ok {
		t.Error("an unknown key was throttled; authentication decides that, not this")
	}
}

func TestForgetResetsAKey(t *testing.T) {
	now := time.Now()
	l := limiter(map[string]float64{"key-a": 1})
	atTime(l, &now)

	for range 2 {
		l.Allow("key-a")
	}
	l.Forget("key-a")
	if ok, _ := l.Allow("key-a"); !ok {
		t.Error("Forget did not reset the bucket")
	}
}

// Advising "retry in 0 seconds" is advising an immediate retry, which is the
// request that just got refused.
func TestRetryAfterIsNeverZero(t *testing.T) {
	for _, wait := range []time.Duration{0, time.Millisecond, 400 * time.Millisecond} {
		if got := RetryAfterSeconds(wait); got < 1 {
			t.Errorf("RetryAfterSeconds(%v) = %d", wait, got)
		}
	}
	if got := RetryAfterSeconds(2500 * time.Millisecond); got != 3 {
		t.Errorf("RetryAfterSeconds(2.5s) = %d, want 3", got)
	}
}

func TestRateLimitMiddlewareRefusesWithRetryAfter(t *testing.T) {
	now := time.Now()
	l := limiter(map[string]float64{"key-a": 1})
	atTime(l, &now)

	handler := RateLimit(l, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	call := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
		r = r.WithContext(core.WithCaller(r.Context(), core.Caller{KeyID: "key-a"}))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec
	}

	for i := range burstFactor {
		if got := call().Code; got != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i+1, got)
		}
	}
	rec := call()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
}

// Open mode has no keys, so there is nothing to attribute a rate to.
func TestRateLimitLetsUnauthenticatedTrafficThrough(t *testing.T) {
	l := limiter(map[string]float64{"": 1})
	handler := RateLimit(l, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := range 5 {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/models", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i+1, rec.Code)
		}
	}
}

func TestRateLimitUsesTheSuppliedDenier(t *testing.T) {
	now := time.Now()
	l := limiter(map[string]float64{"key-a": 1})
	atTime(l, &now)

	handler := RateLimit(l, func(w http.ResponseWriter, _ *http.Request, wait time.Duration) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusTooManyRequests)
	})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))

	var rec *httptest.ResponseRecorder
	for range burstFactor + 1 {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
		r = r.WithContext(core.WithCaller(r.Context(), core.Caller{KeyID: "key-a"}))
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want the dialect's own shape", got)
	}
}
