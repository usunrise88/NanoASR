package main

import (
	"io"
	"os"
	"path/filepath"
	"sync"
)

// maxLogBytes is the size at which the log file rolls over. One previous
// generation is kept, so the log costs at most twice this on disk however long
// the service runs. A service nobody is watching is exactly the one that must
// not fill the disk.
const maxLogBytes = 64 << 20

// openLog decides where the server writes its log.
//
// An empty path means stderr, which is right for a console run and impossible
// for a Windows service: a service has no console, so everything written to
// stderr is discarded and a service that fails to start leaves nothing to read.
// That is why `nanoasr service install` always registers a -log-file.
func openLog(path string) (io.Writer, func(), error) {
	if path == "" {
		return os.Stderr, func() {}, nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, err
		}
	}
	f, err := newRollingFile(path, maxLogBytes)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}

// rollingFile is an append-only file that renames itself out of the way once
// it passes a size, keeping one previous generation.
//
// It is deliberately the smallest thing that stops a log from growing without
// bound: no time-based rotation, no compression, no external logrotate to
// configure on a Windows box that has none.
type rollingFile struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func newRollingFile(path string, max int64) (*rollingFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, err
	}
	size := int64(0)
	if st, err := f.Stat(); err == nil {
		size = st.Size()
	}
	return &rollingFile{path: path, max: max, f: f, size: size}, nil
}

func (r *rollingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.size > 0 && r.size+int64(len(p)) > r.max {
		// A rotation that fails is not worth losing the line over: the write
		// below still goes to the file that is open, which then grows past the
		// limit rather than disappearing.
		r.rotate()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate must be called with the lock held. The file is closed before the
// rename because Windows refuses to rename an open file, and the previous
// generation is removed first because a rename onto an existing name fails
// there as well.
func (r *rollingFile) rotate() {
	prev := r.path + ".1"
	if err := r.f.Close(); err != nil {
		return
	}
	_ = os.Remove(prev)
	if err := os.Rename(r.path, prev); err != nil {
		// Reopen what is still there rather than leaving the writer closed.
		if f, oerr := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640); oerr == nil {
			r.f = f
		}
		return
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		// The rename succeeded, so the old file is safe under .1; reopening is
		// the only thing that failed. Put it back so writes keep landing.
		_ = os.Rename(prev, r.path)
		if f, oerr := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640); oerr == nil {
			r.f = f
		}
		return
	}
	r.f = f
	r.size = 0
}

func (r *rollingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
