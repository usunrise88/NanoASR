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
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxAPIKeyID
)

// RequestID returns the id assigned to this request.
func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(ctxRequestID).(string)
	return s
}

// APIKeyID returns the authenticated key id, empty in open mode.
func APIKeyID(ctx context.Context) string {
	s, _ := ctx.Value(ctxAPIKeyID).(string)
	return s
}

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

// Auth enforces bearer authentication. In open mode it is not installed at all,
// and config.Validate refuses to start open mode on a non-loopback address.
func Auth(keys KeyStore) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxAPIKeyID, id)))
		})
	}
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
