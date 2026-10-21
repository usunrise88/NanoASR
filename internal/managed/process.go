package managed

import (
	"errors"
	"io"
)

// ErrLocked is a data directory another instance is running in.
var ErrLocked = errors.New("another nanoasr instance is using this data directory")

// WatchStdin calls stop when r reaches its end.
//
// The application holds the write end of the pipe on the instance's standard
// input and never writes to it. When the application exits — cleanly,
// killed, crashed — the operating system closes that end, and this is how the
// instance finds out on every platform without polling. Anything written to
// it is read and ignored.
func WatchStdin(r io.Reader, stop func(reason string)) {
	go func() {
		_, _ = io.Copy(io.Discard, r)
		stop("stdin closed")
	}()
}
