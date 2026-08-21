package core

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is a stable, machine-readable error identifier. Dialects map it to their
// own wire format; the set itself never depends on a dialect.
type Code string

const (
	CodeInvalidRequest        Code = "invalid_request"
	CodeUnauthorized          Code = "unauthorized"
	CodeModelForbidden        Code = "model_forbidden"
	CodeModelNotFound         Code = "model_not_found"
	CodeJobNotFound           Code = "job_not_found"
	CodeFileTooLarge          Code = "file_too_large"
	CodeDurationExceeded      Code = "duration_exceeded"
	CodeUnsupportedMediaType  Code = "unsupported_media_type"
	CodeCapabilityUnavailable Code = "capability_unavailable"
	CodeQueueFull             Code = "queue_full"
	CodeRateLimited           Code = "rate_limited"
	CodeProcessingTimeout     Code = "processing_timeout"
	CodeModelUnavailable      Code = "model_unavailable"
	CodeDraining              Code = "draining"
	CodeInternal              Code = "internal"
	CodeNotImplemented        Code = "not_implemented"
)

// Error is the single error type crossing the service boundary.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param,omitempty"`
	cause   error
}

func (e *Error) Error() string {
	if e.Param != "" {
		return fmt.Sprintf("%s: %s (param %s)", e.Code, e.Message, e.Param)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

// Errorf builds a domain error.
func Errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithParam names the offending request field.
func (e *Error) WithParam(p string) *Error { e.Param = p; return e }

// WithCause attaches an internal cause. The cause is logged, never returned to
// a client.
func (e *Error) WithCause(err error) *Error { e.cause = err; return e }

// HTTPStatus is the status a dialect should use unless it has a reason not to.
func (c Code) HTTPStatus() int {
	switch c {
	case CodeInvalidRequest:
		return http.StatusBadRequest
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeModelForbidden:
		return http.StatusForbidden
	case CodeModelNotFound, CodeJobNotFound:
		return http.StatusNotFound
	case CodeFileTooLarge, CodeDurationExceeded:
		return http.StatusRequestEntityTooLarge
	case CodeUnsupportedMediaType:
		return http.StatusUnsupportedMediaType
	case CodeCapabilityUnavailable:
		return http.StatusUnprocessableEntity
	case CodeQueueFull, CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeProcessingTimeout:
		return http.StatusGatewayTimeout
	case CodeModelUnavailable, CodeDraining:
		return http.StatusServiceUnavailable
	case CodeNotImplemented:
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

// AsError extracts a domain error, or wraps an unknown one as internal.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return Errorf(CodeInternal, "internal error").WithCause(err)
}

// ErrNotImplemented marks skeleton seams that a milestone still has to fill.
var ErrNotImplemented = Errorf(CodeNotImplemented, "not implemented")
