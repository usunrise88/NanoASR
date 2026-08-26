package sqlite

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver; registers itself as "sqlite"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/job"
)

// schemaVersion is bumped whenever Schema changes shape. Migrations are
// numbered and applied by hand; there is no generator on purpose.
const schemaVersion = 1

// defaultLimit caps a history page when the caller does not say.
const defaultLimit = 50

// maxLimit is what a caller can ask for at most. A history page is JSON with
// full transcripts inside, so an unbounded limit is an unbounded response.
const maxLimit = 200

// Store implements job.Store on SQLite.
type Store struct {
	db   *sql.DB
	path string
}

// Open connects, applies pragmas and runs migrations.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("sqlite: empty database path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := ensureDir(dir); err != nil {
			return nil, err
		}
	}

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	// A single writer with WAL readers is what this workload is: one queue
	// writing transitions, a handful of handlers reading history.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)

	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// dsn spells the pragmas into the connection string so every pooled connection
// gets them. See the comment on Pragmas.
func dsn(path string) string {
	q := url.Values{}
	for _, p := range Pragmas {
		q.Add("_pragma", p)
	}
	// _txlock=immediate takes the write lock when the transaction starts
	// instead of on its first write, which is what turns a deferred upgrade
	// deadlock between two writers into a plain busy-timeout wait.
	q.Set("_txlock", "immediate")
	return "file:" + path + "?" + q.Encode()
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(Schema); err != nil {
		return fmt.Errorf("sqlite: apply schema: %w", err)
	}
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES(?, ?)`,
		schemaVersion, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("sqlite: record migration: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Path is where the database lives; the CLI reports it.
func (s *Store) Path() string { return s.path }

func (s *Store) Create(ctx context.Context, r job.Record) error {
	params, err := json.Marshal(r.Params)
	if err != nil {
		return fmt.Errorf("sqlite: encode params: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO jobs (id, status, priority, model_id, model_rev, params_json, source,
                  api_key_id, filename, audio_bytes, audio_seconds, created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.Job.ID, string(r.Job.Status), int(r.Priority), r.Job.ModelID, r.Job.ModelRev,
		string(params), string(r.Job.Source), nullString(r.APIKeyID),
		nullString(r.Job.Filename), r.AudioBytes, r.AudioSeconds,
		r.Job.CreatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("sqlite: create job %s: %w", r.Job.ID, err)
	}
	return nil
}

// Update writes a state transition. It never rewrites the immutable columns —
// params, key, creation time — so a stale in-memory copy cannot resurrect them.
func (s *Store) Update(ctx context.Context, r job.Record) error {
	var resultJSON, statsJSON any
	if r.Job.Result != nil {
		encoded, err := json.Marshal(r.Job.Result)
		if err != nil {
			return fmt.Errorf("sqlite: encode result: %w", err)
		}
		resultJSON = string(encoded)

		stats, err := json.Marshal(r.Job.Result.Stats)
		if err != nil {
			return fmt.Errorf("sqlite: encode stats: %w", err)
		}
		statsJSON = string(stats)
	}

	var code, message any
	if r.Job.Error != nil {
		code = string(r.Job.Error.Code)
		message = r.Job.Error.Message
	}

	res, err := s.db.ExecContext(ctx, `
UPDATE jobs SET status=?, model_id=?, model_rev=?, audio_seconds=?,
                started_at=?, finished_at=?, error_code=?, error_message=?,
                result_json=?, stats_json=?
WHERE id=?`,
		string(r.Job.Status), r.Job.ModelID, r.Job.ModelRev, r.AudioSeconds,
		nullTime(r.Job.StartedAt), nullTime(r.Job.FinishedAt),
		code, message, resultJSON, statsJSON, r.Job.ID)
	if err != nil {
		return fmt.Errorf("sqlite: update job %s: %w", r.Job.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.Errorf(core.CodeJobNotFound, "no such job: %s", r.Job.ID)
	}
	return nil
}

const selectColumns = `id, status, priority, model_id, model_rev, params_json, source,
       api_key_id, filename, audio_bytes, audio_seconds, created_at, started_at,
       finished_at, error_code, error_message, result_json`

// selectColumnsNoResult is what the history listing reads when the caller
// asked with include_result=false: every row carries the full transcript by
// default, and a page of long jobs is hundreds of megabytes of JSON for
// nothing the UI needs to render a list.
const selectColumnsNoResult = `id, status, priority, model_id, model_rev, params_json, source,
       api_key_id, filename, audio_bytes, audio_seconds, created_at, started_at,
       finished_at, error_code, error_message`

func (s *Store) Get(ctx context.Context, id string) (job.Record, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+selectColumns+` FROM jobs WHERE id=?`, id)
	r, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return job.Record{}, core.Errorf(core.CodeJobNotFound, "no such job: %s", id)
	}
	if err != nil {
		return job.Record{}, fmt.Errorf("sqlite: get job %s: %w", id, err)
	}
	return r, nil
}

func (s *Store) List(ctx context.Context, f core.JobFilter) ([]core.Job, string, error) {
	where := []string{"1=1"}
	var args []any

	// The ownership rule. An administrator sees everything; anyone else sees
	// the jobs their own key created.
	if !f.Admin {
		where = append(where, "api_key_id IS ?")
		args = append(args, nullString(f.APIKeyID))
	}
	if len(f.Status) > 0 {
		where = append(where, "status IN ("+placeholders(len(f.Status))+")")
		for _, st := range f.Status {
			args = append(args, string(st))
		}
	}
	if f.ModelID != "" {
		where = append(where, "model_id = ?")
		args = append(args, f.ModelID)
	}
	if f.Source != "" {
		where = append(where, "source = ?")
		args = append(args, string(f.Source))
	}
	if f.Since != nil {
		where = append(where, "created_at >= ?")
		args = append(args, f.Since.UnixMilli())
	}
	if f.Cursor != "" {
		createdAt, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", err
		}
		// Composite comparison, because created_at alone is not unique: two
		// jobs submitted in the same millisecond would make a page boundary
		// either skip one or repeat one.
		where = append(where, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, createdAt, createdAt, id)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	// One extra row answers "is there a next page" without a second count query.
	cols := selectColumns
	if f.OmitResult {
		cols = selectColumnsNoResult
	}
	query := `SELECT ` + cols + ` FROM jobs WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY created_at DESC, id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, append(args, limit+1)...)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: list jobs: %w", err)
	}
	defer rows.Close()

	out := make([]core.Job, 0, limit)
	var next string
	var scan func(scanner) (job.Record, error)
	if f.OmitResult {
		scan = scanRecordSummary
	} else {
		scan = scanRecord
	}
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, "", fmt.Errorf("sqlite: list jobs: %w", err)
		}
		if len(out) == limit {
			next = encodeCursor(out[len(out)-1])
			break
		}
		out = append(out, r.Job)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: list jobs: %w", err)
	}
	return out, next, nil
}

func (s *Store) Pending(ctx context.Context) ([]job.Record, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+selectColumns+` FROM jobs WHERE status=? ORDER BY created_at ASC, id ASC`,
		string(core.JobQueued))
	if err != nil {
		return nil, fmt.Errorf("sqlite: list pending jobs: %w", err)
	}
	defer rows.Close()

	var out []job.Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list pending jobs: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) FailStale(ctx context.Context, reason string) (int, error) {
	res, err := s.db.ExecContext(ctx, `
UPDATE jobs SET status=?, error_code=?, error_message=?, finished_at=?
WHERE status=?`,
		string(core.JobFailed), "server_restart", reason,
		time.Now().UnixMilli(), string(core.JobRunning))
	if err != nil {
		return 0, fmt.Errorf("sqlite: fail stale jobs: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Store) Purge(ctx context.Context, ttl time.Duration) (int, error) {
	if ttl <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-ttl).UnixMilli()
	// COALESCE, because a job that never started has no finished_at either and
	// would otherwise sit in history forever.
	res, err := s.db.ExecContext(ctx, `
DELETE FROM jobs
WHERE status IN (?,?,?,?) AND COALESCE(finished_at, created_at) < ?`,
		string(core.JobSucceeded), string(core.JobFailed),
		string(core.JobCanceled), string(core.JobExpired), cutoff)
	if err != nil {
		return 0, fmt.Errorf("sqlite: purge history: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// scanner is what both *sql.Row and *sql.Rows satisfy.
type scanner interface{ Scan(dest ...any) error }

func scanRecord(sc scanner) (job.Record, error) {
	var (
		r          job.Record
		status     string
		priority   int
		params     string
		source     string
		apiKeyID   sql.NullString
		filename   sql.NullString
		createdAt  int64
		startedAt  sql.NullInt64
		finishedAt sql.NullInt64
		errCode    sql.NullString
		errMessage sql.NullString
		resultJSON sql.NullString
	)
	err := sc.Scan(&r.Job.ID, &status, &priority, &r.Job.ModelID, &r.Job.ModelRev,
		&params, &source, &apiKeyID, &filename, &r.AudioBytes, &r.AudioSeconds,
		&createdAt, &startedAt, &finishedAt, &errCode, &errMessage, &resultJSON)
	if err != nil {
		return job.Record{}, err
	}

	r.Job.Status = core.JobStatus(status)
	r.Job.Source = core.Source(source)
	r.Job.Filename = filename.String
	r.Job.CreatedAt = time.UnixMilli(createdAt)
	r.Priority = job.Priority(priority)
	r.APIKeyID = apiKeyID.String

	if startedAt.Valid {
		t := time.UnixMilli(startedAt.Int64)
		r.Job.StartedAt = &t
	}
	if finishedAt.Valid {
		t := time.UnixMilli(finishedAt.Int64)
		r.Job.FinishedAt = &t
	}
	if errCode.Valid {
		r.Job.Error = core.Errorf(core.Code(errCode.String), "%s", errMessage.String)
	}
	if resultJSON.Valid && resultJSON.String != "" {
		var res core.Result
		if err := json.Unmarshal([]byte(resultJSON.String), &res); err != nil {
			return job.Record{}, fmt.Errorf("decode result of %s: %w", r.Job.ID, err)
		}
		r.Job.Result = &res
	}
	if params != "" {
		if err := json.Unmarshal([]byte(params), &r.Params); err != nil {
			return job.Record{}, fmt.Errorf("decode params of %s: %w", r.Job.ID, err)
		}
	}
	return r, nil
}

// scanRecordSummary is scanRecord without the result column. It is the
// counterpart of selectColumnsNoResult and leaves Job.Result nil: the detail
// endpoint can fetch the full record on demand.
func scanRecordSummary(sc scanner) (job.Record, error) {
	var (
		r          job.Record
		status     string
		priority   int
		params     string
		source     string
		apiKeyID   sql.NullString
		filename   sql.NullString
		createdAt  int64
		startedAt  sql.NullInt64
		finishedAt sql.NullInt64
		errCode    sql.NullString
		errMessage sql.NullString
	)
	err := sc.Scan(&r.Job.ID, &status, &priority, &r.Job.ModelID, &r.Job.ModelRev,
		&params, &source, &apiKeyID, &filename, &r.AudioBytes, &r.AudioSeconds,
		&createdAt, &startedAt, &finishedAt, &errCode, &errMessage)
	if err != nil {
		return job.Record{}, err
	}

	r.Job.Status = core.JobStatus(status)
	r.Job.Source = core.Source(source)
	r.Job.Filename = filename.String
	r.Job.CreatedAt = time.UnixMilli(createdAt)
	r.Priority = job.Priority(priority)
	r.APIKeyID = apiKeyID.String

	if startedAt.Valid {
		t := time.UnixMilli(startedAt.Int64)
		r.Job.StartedAt = &t
	}
	if finishedAt.Valid {
		t := time.UnixMilli(finishedAt.Int64)
		r.Job.FinishedAt = &t
	}
	if errCode.Valid {
		r.Job.Error = core.Errorf(core.Code(errCode.String), "%s", errMessage.String)
	}
	if params != "" {
		if err := json.Unmarshal([]byte(params), &r.Params); err != nil {
			return job.Record{}, fmt.Errorf("decode params of %s: %w", r.Job.ID, err)
		}
	}
	return r, nil
}

// encodeCursor pins a page boundary to a row rather than to an offset: rows
// inserted while a client pages would shift every OFFSET after them.
func encodeCursor(j core.Job) string {
	raw := strconv.FormatInt(j.CreatedAt.UnixMilli(), 10) + ":" + j.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(cursor string) (int64, string, error) {
	invalid := core.Errorf(core.CodeInvalidRequest, "malformed cursor").WithParam("cursor")

	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, "", invalid
	}
	createdAt, id, ok := strings.Cut(string(raw), ":")
	if !ok || id == "" {
		return 0, "", invalid
	}
	ms, err := strconv.ParseInt(createdAt, 10, 64)
	if err != nil {
		return 0, "", invalid
	}
	return ms, id, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

var _ job.Store = (*Store)(nil)
