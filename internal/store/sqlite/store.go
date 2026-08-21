package sqlite

import (
	"context"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/job"
)

// Store implements job.Store on SQLite.
type Store struct {
	path string
}

// Open connects, applies pragmas and runs migrations.
//
// TODO(M3): import modernc.org/sqlite, open the database, apply Pragmas and
// Schema, record the migration, and mark stale running jobs as failed.
func Open(path string) (*Store, error) {
	return &Store{path: path}, core.ErrNotImplemented
}

func (s *Store) Create(context.Context, core.Job) error { return core.ErrNotImplemented }
func (s *Store) Update(context.Context, core.Job) error { return core.ErrNotImplemented }

func (s *Store) Get(context.Context, string) (core.Job, error) {
	return core.Job{}, core.ErrNotImplemented
}

func (s *Store) List(context.Context, core.JobFilter) ([]core.Job, error) {
	return nil, core.ErrNotImplemented
}

func (s *Store) FailStale(context.Context, string) (int, error) { return 0, core.ErrNotImplemented }

func (s *Store) Purge(context.Context, time.Duration) (int, error) {
	return 0, core.ErrNotImplemented
}

func (s *Store) Close() error { return nil }

var _ job.Store = (*Store)(nil)
