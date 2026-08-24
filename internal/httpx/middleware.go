// Package httpx holds transport concerns shared by every dialect: request ids,
// panic recovery, authentication, body limits and CORS.
//
// Dialects do not implement any of this. Keeping it here is what makes a custom
// dialect a small file rather than a security review.
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

type ctxKey int

const ctxRequestID ctxKey = iota

// RequestID returns the id assigned to this request.
func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(ctxRequestID).(string)
	return s
}

// APIKeyID returns the authenticated key id, empty in open mode.
func APIKeyID(ctx context.Context) string { return core.CallerOf(ctx).KeyID }

// IsAdmin reports whether the authenticated key may perform administrative
// operations. Open mode has no keys and therefore no restrictions.
func IsAdmin(ctx context.Context) bool { return core.CallerOf(ctx).Admin }

// Middleware is the standard decorator shape.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware so that the first argument is the outermost layer,
// which is also the order they appear in SPEC §4.
func Chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// WithRequestID assigns an id, echoes it and puts it in the context so logs and
// error responses can be correlated.
func WithRequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-Id")
			if id == "" {
				id = newID()
			}
			w.Header().Set("X-Request-Id", id)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
		})
	}
}

// Recover turns a panic into a 500 instead of a dropped connection, and logs
// the stack once.
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					log.Error("panic serving request",
						"request_id", RequestID(r.Context()),
						"path", r.URL.Path,
						"panic", v,
						"stack", string(debug.Stack()))
					http.Error(w, `{"error":{"code":"internal","message":"internal error"}}`,
						http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// LimitBody caps the request body before any handler reads it, so an oversized
// upload is rejected without being buffered first.
func LimitBody(maxBytes int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}

// KeyStore verifies bearer tokens. Implementations must compare in constant
// time and must never log the token.
type KeyStore interface {
	Verify(ctx context.Context, token string) (keyID string, ok bool)
}

// adminLookup is an optional KeyStore capability: a store that can say whether
// a verified key is administrative.
type adminLookup interface {
	Lookup(id string) (Key, bool)
}

// Auth enforces bearer authentication on everything except publicPrefixes.
//
// The exemptions are passed in rather than hard-coded, and they are one list in
// one call so a reviewer sees the whole unauthenticated surface at once. Health
// probes must be on it — a readiness check that needs a credential is a
// readiness check Kubernetes cannot make — and so must the UI assets, because a
// browser does not send a bearer token when loading a script tag.
//
// Open mode does not install this middleware at all, and config.Validate
// refuses open mode on a non-loopback address.
func Auth(keys KeyStore, publicPrefixes ...string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isPublicPath(r.URL.Path, publicPrefixes) {
				next.ServeHTTP(w, r)
				return
			}

			token := bearer(r.Header.Get("Authorization"))
			if token == "" {
				unauthorized(w)
				return
			}
			id, ok := keys.Verify(r.Context(), token)
			if !ok {
				unauthorized(w)
				return
			}

			caller := core.Caller{KeyID: id}
			if l, ok := keys.(adminLookup); ok {
				if k, found := l.Lookup(id); found {
					caller.Admin = k.Admin
					caller.Interactive = k.Interactive
				}
			}
			ctx := core.WithCaller(r.Context(), caller)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireAdmin guards operations that change server state rather than just
// reading it: loading, unloading and downloading models.
//
// deny renders the refusal. It is a parameter because the error shape belongs to
// the dialect — problem+json for native, OpenAI's envelope for openai — and a
// 403 in the wrong shape is a 403 the client's error handling does not
// recognise. Passing nil takes the plain JSON default.
func RequireAdmin(deny http.HandlerFunc) Middleware {
	if deny == nil {
		deny = denyAdmin
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !IsAdmin(r.Context()) {
				deny(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func denyAdmin(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	http.Error(w,
		`{"error":{"code":"model_forbidden","message":"this API key is not permitted to administer models"}}`,
		http.StatusForbidden)
}

// isPublicPath matches a prefix exactly, or as a path segment boundary, so
// "/ui" exempts "/ui" and "/ui/app.js" but never "/uisecret".
func isPublicPath(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if p == "" {
			continue
		}
		p = strings.TrimSuffix(p, "/")
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

func bearer(h string) string {
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="nanoasr"`)
	http.Error(w, `{"error":{"code":"unauthorized","message":"missing or invalid API key"}}`,
		http.StatusUnauthorized)
}

// SecurityHeaders sets the handful of headers that cost nothing and prevent a
// class of mistakes.
func SecurityHeaders() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
			next.ServeHTTP(w, r)
		})
	}
}

// AccessLog logs one line per request. Bodies, filenames and transcripts are
// never logged (SPEC §11).
func AccessLog(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			log.Info("request",
				"request_id", RequestID(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"bytes", sw.bytes,
				"duration_ms", time.Since(start).Milliseconds())
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Flush lets SSE handlers stream through the logging wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
