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
	// Interactive lets this caller's work overtake a batch backlog.
	//
	// It is a property of the key rather than of the request because a request
	// parameter would let any client declare itself urgent, which is the same
	// as having no priorities at all. The operator decides which key is a
	// person waiting at a screen.
	Interactive bool
}

type callerKey struct{}

// WithCaller attaches the authenticated identity.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerOf returns the identity behind a request. Open mode has no keys and no
// restrictions, so the default is an unnamed administrator whose work is
// interactive — that mode only runs on loopback, where the caller is a person.
func CallerOf(ctx context.Context) Caller {
	if c, ok := ctx.Value(callerKey{}).(Caller); ok {
		return c
	}
	return Caller{Admin: true, Interactive: true}
}
