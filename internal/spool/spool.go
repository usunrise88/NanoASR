// Package spool owns the audio of a queued job: where it lives, how much of it
// the server is willing to hold, and when it is deleted.
//
// It exists as its own package because both sides need it and neither should
// depend on the other: an API dialect hands an upload over, and the queue takes
// it from there.
package spool

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Name is the on-disk name of a job's audio.
//
// It is derived from the job id rather than kept in a column: that is what lets
// a restart pair a queued row with its file, and lets startup cleanup recognise
// a file whose job is gone. The extension carries no meaning — the decoder
// sniffs magic bytes — so nothing depends on the client's filename.
func Name(jobID string) string { return "job-" + jobID + ".audio" }

// JobID is the inverse, and reports whether name is a spool file at all.
func JobID(name string) (string, bool) {
	const prefix, suffix = "job-", ".audio"
	if len(name) <= len(prefix)+len(suffix) {
		return "", false
	}
	if name[:len(prefix)] != prefix || name[len(name)-len(suffix):] != suffix {
		return "", false
	}
	return name[len(prefix) : len(name)-len(suffix)], true
}

// FileSource is an upload that outlived the request that carried it.
//
// The synchronous path never needs this: net/http's own spool is removed when
// the handler returns, which is exactly right when the response carries the
// transcript. A queued job is different — the handler returns 202 long before
// the work starts — so the file has to change owners rather than be deleted.
type FileSource struct {
	path     string
	filename string
	size     int64
}

func NewFileSource(path, filename string, size int64) *FileSource {
	return &FileSource{path: path, filename: filename, size: size}
}

func (s *FileSource) Open() (io.ReadCloser, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "the queued audio is gone").WithCause(err)
	}
	return f, nil
}

func (s *FileSource) Filename() string { return s.filename }
func (s *FileSource) Size() int64      { return s.size }
func (s *FileSource) Path() string     { return s.path }

// Close is a no-op: the job owns the file, and a handler that merely read it
// must not delete it. Removal goes through Spool.Remove, deliberately and once.
func (s *FileSource) Close() error { return nil }

// Spool is the disk budget for queued audio.
//
// A queued job owns its upload from the moment it is accepted until it reaches
// a terminal state, which is the only way an asynchronous queue can work at all.
// That turns queue_size into a disk commitment: a hundred slots at a hundred
// megabytes each is ten gigabytes. So the queue is bounded twice — by item count
// and by bytes — because the two limits catch different shapes of load, and a
// caller that trips either gets 429 rather than a full disk.
type Spool struct {
	dir string
	max int64

	mu   sync.Mutex
	used int64
	// held maps job id to the bytes reserved for it, so releasing twice — a
	// cancellation racing a completion, say — cannot drive the counter negative.
	held map[string]int64
}

func New(dir string, max int64) *Spool {
	return &Spool{dir: dir, max: max, held: map[string]int64{}}
}

func (s *Spool) Dir() string { return s.dir }

// Max is the configured budget, 0 meaning unlimited.
func (s *Spool) Max() int64 { return s.max }

// Path is where a job's audio lives.
func (s *Spool) Path(jobID string) string { return filepath.Join(s.dir, Name(jobID)) }

// Reserve claims n bytes for a job. A budget of zero means unlimited.
func (s *Spool) Reserve(jobID string, n int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, dup := s.held[jobID]; dup {
		return fmt.Errorf("spool: job %s already holds a reservation", jobID)
	}
	if s.max > 0 && s.used+n > s.max {
		return core.Errorf(core.CodeQueueFull,
			"the queue is already holding %d of %d bytes of audio; retry shortly",
			s.used, s.max)
	}
	s.used += n
	s.held[jobID] = n
	return nil
}

// Release gives the bytes back. It is idempotent.
func (s *Spool) Release(jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, ok := s.held[jobID]
	if !ok {
		return
	}
	s.used -= n
	delete(s.held, jobID)
}

// Remove deletes a job's audio and releases its bytes. This is the one place
// queued audio is deleted, so "when does the file go away" has a single answer.
func (s *Spool) Remove(jobID string) {
	defer s.Release(jobID)
	if err := os.Remove(s.Path(jobID)); err != nil && !os.IsNotExist(err) {
		slog.Warn("could not remove spooled audio", "job", jobID, "err", err)
	}
}

// Used is the reserved total, for /readyz and tests.
func (s *Spool) Used() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used
}

// Sweep reconciles the directory with the database at startup.
//
// Jobs in live — those still queued — are re-reserved: they are about to run and
// their audio must count against the budget. Every other spool file is an
// orphan: a process killed between the 202 and the database write, or a job that
// FailStale has just given up on. Order matters, and it is the caller's job to
// get right: sweeping before the queue has recovered its pending work deletes
// exactly the files that were resumable.
func (s *Spool) Sweep(live map[string]int64) (removed int, err error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("spool: read %s: %w", s.dir, err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id, ok := JobID(e.Name())
		if !ok {
			// Not ours. The temp directory is shared with ffmpeg and with
			// whatever else the operator points at it.
			continue
		}
		if size, alive := live[id]; alive {
			if err := s.Reserve(id, size); err != nil {
				slog.Warn("could not re-reserve resumed audio", "job", id, "err", err)
			}
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil && !os.IsNotExist(err) {
			slog.Warn("could not remove orphaned audio", "file", e.Name(), "err", err)
			continue
		}
		removed++
	}
	return removed, nil
}

// Detachable is an upload that can hand its own storage over — net/http's
// multipart spool being the one that matters. The interface lives here rather
// than in the API layer so the queue can take ownership without knowing what a
// multipart form is.
type Detachable interface {
	core.AudioSource
	Detach(dir, name string) (*FileSource, error)
}

// Adopt takes ownership of a request's audio on behalf of a job.
//
// An upload that can move itself does; anything else is copied. Either way the
// result is a file at a path derived from the job id, which is what makes the
// job resumable and the leftovers recognisable.
func (s *Spool) Adopt(jobID string, src core.AudioSource) (*FileSource, error) {
	if src == nil {
		return nil, core.Errorf(core.CodeInvalidRequest, "no audio in the request")
	}
	if d, ok := src.(Detachable); ok {
		return d.Detach(s.dir, Name(jobID))
	}
	if f, ok := src.(*FileSource); ok && f.path == s.Path(jobID) {
		return f, nil // already ours
	}
	return s.copy(jobID, src)
}

func (s *Spool) copy(jobID string, src core.AudioSource) (*FileSource, error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, core.Errorf(core.CodeInternal, "cannot prepare the spool directory").WithCause(err)
	}
	in, err := src.Open()
	if err != nil {
		return nil, err
	}
	defer in.Close()

	dst := s.Path(jobID)
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "cannot spool the audio").WithCause(err)
	}
	n, err := io.Copy(out, in)
	if err == nil {
		err = out.Close()
	} else {
		out.Close()
	}
	if err != nil {
		_ = os.Remove(dst)
		return nil, core.Errorf(core.CodeInternal, "cannot spool the audio").WithCause(err)
	}
	return NewFileSource(dst, src.Filename(), n), nil
}

// Source reopens a job's spooled audio from its stored metadata.
func (s *Spool) Source(jobID, filename string, size int64) *FileSource {
	return NewFileSource(s.Path(jobID), filename, size)
}

var _ core.AudioSource = (*FileSource)(nil)
