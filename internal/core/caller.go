package core

import "context"

// Caller is who is asking. It rides in the context because the ownership rule —
// a key sees its own jobs, an administrator sees all of them — has to hold for
// every dialect, including one written later by someone who never read this
// file. Passing it as a parameter would make forgetting it a compile-time
// success.
//
// The transport fills it in (httpx.Auth); the service reads it.
type Caller struct {
	// KeyID identifies the API key, or is empty in open mode.
	KeyID string
	// Admin permits administering models and seeing every job.
	Admin bool
}

type callerKey struct{}

// WithCaller attaches the authenticated identity.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerOf returns the identity behind a request. Open mode has no keys and no
// restrictions, so the zero value is an unnamed administrator.
func CallerOf(ctx context.Context) Caller {
	if c, ok := ctx.Value(callerKey{}).(Caller); ok {
		return c
	}
	return Caller{Admin: true}
}
