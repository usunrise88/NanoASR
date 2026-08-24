// Package sqlite persists jobs and API keys.
//
// Driver: modernc.org/sqlite (pure Go). cgo is already in play through
// sherpa-onnx, and adding a second cgo dependency would tie the build to a
// second C toolchain for no benefit.
package sqlite

// Schema is applied at startup and is idempotent. Migrations are numbered and
// recorded in schema_migrations; there is no ORM and no automatic migration
// generation on purpose.
//
// Note what is not here: no audio, and no blob table. Uploaded audio never
// outlives the active job (SPEC §2.1), so job history is metadata and text.
const Schema = `
CREATE TABLE IF NOT EXISTS jobs (
  id            TEXT PRIMARY KEY,
  status        TEXT NOT NULL,
  priority      INTEGER NOT NULL DEFAULT 0,
  model_id      TEXT NOT NULL,
  model_rev     TEXT NOT NULL,
  params_json   TEXT NOT NULL,
  source        TEXT NOT NULL,
  api_key_id    TEXT,
  filename      TEXT,
  audio_bytes   INTEGER,
  audio_seconds REAL,
  created_at    INTEGER NOT NULL,
  started_at    INTEGER,
  finished_at   INTEGER,
  error_code    TEXT,
  error_message TEXT,
  result_json   TEXT,
  stats_json    TEXT
);
CREATE INDEX IF NOT EXISTS jobs_status_created ON jobs(status, created_at DESC);
CREATE INDEX IF NOT EXISTS jobs_model          ON jobs(model_id, created_at DESC);

CREATE TABLE IF NOT EXISTS api_keys (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  hash       TEXT NOT NULL,
  scopes     TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_used  INTEGER,
  disabled   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  applied_at INTEGER NOT NULL
);
`

// Pragmas are applied to every connection, and that is why they are written in
// the driver's DSN form rather than as PRAGMA statements: database/sql keeps a
// pool, and an Exec at open time configures exactly one connection out of it.
// The next query could then run on a connection with neither WAL nor a busy
// timeout, which fails as an intermittent "database is locked" rather than as
// anything pointing back here.
//
// WAL is what lets the history page read while a worker writes.
var Pragmas = []string{
	"journal_mode(WAL)",
	"synchronous(NORMAL)",
	"busy_timeout(5000)",
	"foreign_keys(1)",
}
