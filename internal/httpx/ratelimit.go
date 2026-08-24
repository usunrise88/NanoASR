package httpx

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

// burstFactor is how much of a burst a key may spend at once, as a multiple of
// its per-second rate.
//
// A limit with no burst allowance rejects the perfectly ordinary case of a
// client opening several requests in the same tick, which is what a UI does on
// page load. Two seconds' worth is enough for that and still bounded.
const burstFactor = 2

// RateLimiter throttles per API key.
//
// One process, a handful of keys, so the buckets live in memory. A distributed
// limiter here would be infrastructure chosen for its own sake: this server is a
// single binary by design, and a second one behind a load balancer already needs
// a shared limiter for reasons that have nothing to do with this file.
type RateLimiter struct {
	limits func(id string) (float64, bool)

	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter reads each key's allowance through limits, which returns the
// rate and whether the key is known.
func NewRateLimiter(limits func(id string) (float64, bool)) *RateLimiter {
	return &RateLimiter{
		limits:  limits,
		buckets: map[string]*bucket{},
		now:     time.Now,
	}
}

// Allow reports whether the key may make a request now, and how long to wait if
// not.
func (l *RateLimiter) Allow(id string) (bool, time.Duration) {
	rate, ok := l.limits(id)
	if !ok || rate <= 0 {
		return true, 0
	}
	capacity := rate * burstFactor

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, seen := l.buckets[id]
	if !seen {
		b = &bucket{tokens: capacity, last: now}
		l.buckets[id] = b
	}

	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(capacity, b.tokens+elapsed*rate)
		b.last = now
	}

	if b.tokens < 1 {
		// Round up: advising a wait that is a hair too short guarantees the
		// retry is refused too.
		wait := time.Duration(math.Ceil((1-b.tokens)/rate*1000)) * time.Millisecond
		return false, wait
	}
	b.tokens--
	return true, 0
}

// Forget drops a key's bucket. Used when a key disappears; also lets tests be
// explicit about starting fresh.
func (l *RateLimiter) Forget(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, id)
}

// RateLimit refuses requests from a key that is over its allowance.
//
// It goes after Auth in the chain, because it has nothing to throttle until the
// key is known. Unauthenticated traffic is refused by Auth, which is cheaper
// than any limiter.
//
// deny renders the refusal in the dialect's error shape; nil takes a plain JSON
// default.
func RateLimit(l *RateLimiter, deny func(http.ResponseWriter, *http.Request, time.Duration)) Middleware {
	if deny == nil {
		deny = denyRate
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := core.CallerOf(r.Context()).KeyID
			if id == "" {
				next.ServeHTTP(w, r) // open mode: no key, no allowance to enforce
				return
			}
			if ok, wait := l.Allow(id); !ok {
				deny(w, r, wait)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func denyRate(w http.ResponseWriter, _ *http.Request, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
	w.Header().Set("Content-Type", "application/json")
	http.Error(w,
		`{"error":{"code":"rate_limited","message":"this API key is over its request rate"}}`,
		http.StatusTooManyRequests)
}

// RetryAfterSeconds renders a wait for the Retry-After header, never zero: a
// header saying "retry in 0 seconds" is a header saying "retry immediately".
func RetryAfterSeconds(wait time.Duration) int { return retryAfterSeconds(wait) }

func retryAfterSeconds(wait time.Duration) int {
	seconds := int(math.Ceil(wait.Seconds()))
	if seconds < 1 {
		return 1
	}
	return seconds
}
