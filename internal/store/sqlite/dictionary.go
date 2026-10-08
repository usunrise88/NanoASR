package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

// defaultDictionaryLimit caps a listing that did not ask for a size. There is
// no cursor here on purpose: dictionaries are curated by hand and counted in
// dozens, and a paging API for a list that short would be machinery nobody
// exercises.
const defaultDictionaryLimit = 200

// maxDictionaryMatches is how many matching phrases a search reports per
// dictionary. Enough to show why a dictionary came back, not enough to turn a
// listing into a dump of every list it searched.
const maxDictionaryMatches = 5

// DictionaryStore is core.Dictionaries over the same database the jobs live
// in. A type of its own rather than more methods on Store: the two share a
// connection pool and nothing else, and List and Get already mean something to
// a job.
type DictionaryStore struct {
	db *sql.DB
}

// Dictionaries hands out the dictionary half of this database.
func (s *Store) Dictionaries() *DictionaryStore { return &DictionaryStore{db: s.db} }

// List returns dictionaries without their phrases, newest change first.
//
// The query matches the key, the name, the description and the phrases. Folded
// to lower case in Go and compared against the folded copies in the database:
// SQLite's LIKE folds ASCII only, and a Russian phrase list searched with
// SQLite's own folding would miss every phrase whose case the user did not
// guess.
func (s *DictionaryStore) List(ctx context.Context, query string, limit int) ([]core.Dictionary, error) {
	if limit <= 0 || limit > defaultDictionaryLimit {
		limit = defaultDictionaryLimit
	}
	q := strings.ToLower(strings.TrimSpace(query))

	sqlText := `
SELECT d.key, d.name, d.description, d.score, d.created_at, d.updated_at,
       (SELECT COUNT(*) FROM hotword_phrases p WHERE p.dict_key = d.key)
FROM hotword_dicts d`
	args := []any{}
	if q != "" {
		sqlText += `
WHERE instr(d.search, ?) > 0
   OR EXISTS (SELECT 1 FROM hotword_phrases p
              WHERE p.dict_key = d.key AND instr(p.phrase_lc, ?) > 0)`
		args = append(args, q, q)
	}
	sqlText += `
ORDER BY d.updated_at DESC, d.key
LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list dictionaries: %w", err)
	}
	defer rows.Close()

	var out []core.Dictionary
	for rows.Next() {
		var d core.Dictionary
		var created, updated int64
		if err := rows.Scan(&d.Key, &d.Name, &d.Description, &d.Score,
			&created, &updated, &d.PhraseCount); err != nil {
			return nil, fmt.Errorf("sqlite: scan dictionary: %w", err)
		}
		d.CreatedAt = time.UnixMilli(created).UTC()
		d.UpdatedAt = time.UnixMilli(updated).UTC()
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list dictionaries: %w", err)
	}
	if q == "" || len(out) == 0 {
		return out, nil
	}
	return s.withMatches(ctx, out, q)
}

// withMatches fills in the phrases that made each dictionary match, so a search
// result says why it is a result.
func (s *DictionaryStore) withMatches(ctx context.Context, dicts []core.Dictionary, q string) ([]core.Dictionary, error) {
	keys := make([]any, 0, len(dicts)+1)
	for i := range dicts {
		keys = append(keys, dicts[i].Key)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	// The cap belongs in the query. Twenty dictionaries of five thousand
	// phrases each, searched for "а" on every keystroke, is a hundred thousand
	// rows over the wire to keep a hundred.
	rows, err := s.db.QueryContext(ctx, `
SELECT dict_key, phrase FROM (
  SELECT dict_key, phrase,
         ROW_NUMBER() OVER (PARTITION BY dict_key ORDER BY ord) AS rn
  FROM hotword_phrases
  WHERE dict_key IN (`+placeholders+`) AND instr(phrase_lc, ?) > 0
)
WHERE rn <= ?
ORDER BY dict_key, rn`, append(append(keys, q), maxDictionaryMatches)...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: search phrases: %w", err)
	}
	defer rows.Close()

	hits := map[string][]string{}
	for rows.Next() {
		var key, phrase string
		if err := rows.Scan(&key, &phrase); err != nil {
			return nil, fmt.Errorf("sqlite: scan phrase: %w", err)
		}
		if len(hits[key]) < maxDictionaryMatches {
			hits[key] = append(hits[key], phrase)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: search phrases: %w", err)
	}
	for i := range dicts {
		dicts[i].Matches = hits[dicts[i].Key]
	}
	return dicts, nil
}

// Get returns one dictionary with its phrases, in the order they were saved.
func (s *DictionaryStore) Get(ctx context.Context, key string) (core.Dictionary, error) {
	var d core.Dictionary
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `
SELECT key, name, description, score, created_at, updated_at
FROM hotword_dicts WHERE key = ?`, key).
		Scan(&d.Key, &d.Name, &d.Description, &d.Score, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return d, core.Errorf(core.CodeDictionaryNotFound, "no such dictionary: %s", key)
	}
	if err != nil {
		return d, fmt.Errorf("sqlite: read dictionary %s: %w", key, err)
	}
	d.CreatedAt = time.UnixMilli(created).UTC()
	d.UpdatedAt = time.UnixMilli(updated).UTC()

	rows, err := s.db.QueryContext(ctx,
		`SELECT phrase FROM hotword_phrases WHERE dict_key = ? ORDER BY ord`, key)
	if err != nil {
		return d, fmt.Errorf("sqlite: read phrases of %s: %w", key, err)
	}
	defer rows.Close()
	for rows.Next() {
		var phrase string
		if err := rows.Scan(&phrase); err != nil {
			return d, fmt.Errorf("sqlite: scan phrase: %w", err)
		}
		d.Phrases = append(d.Phrases, phrase)
	}
	if err := rows.Err(); err != nil {
		return d, fmt.Errorf("sqlite: read phrases of %s: %w", key, err)
	}
	d.PhraseCount = len(d.Phrases)
	return d, nil
}

// Save creates or replaces a dictionary.
//
// The phrases are deleted and reinserted rather than diffed. A phrase list is
// edited as a whole, the lists are short, and a diff would buy nothing but a
// way for the stored order to drift from the order the author wrote.
//
// CreatedAt survives a replacement: the dictionary is the same dictionary,
// whatever its contents are now.
func (s *DictionaryStore) Save(ctx context.Context, d core.Dictionary) (core.Dictionary, error) {
	return s.write(ctx, d, false)
}

// Create stores a dictionary only if the key is free, in one statement, so two
// clients racing on the same key cannot both believe they created it.
func (s *DictionaryStore) Create(ctx context.Context, d core.Dictionary) (core.Dictionary, error) {
	return s.write(ctx, d, true)
}

func (s *DictionaryStore) write(ctx context.Context, d core.Dictionary, mustBeNew bool) (core.Dictionary, error) {
	// Millisecond precision, because that is what the column holds: a Save
	// that returned more than the next Get could would differ from itself.
	now := time.Now().UTC().Truncate(time.Millisecond)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return d, fmt.Errorf("sqlite: save dictionary %s: %w", d.Key, err)
	}
	defer func() { _ = tx.Rollback() }()

	var created int64
	err = tx.QueryRowContext(ctx,
		`SELECT created_at FROM hotword_dicts WHERE key = ?`, d.Key).Scan(&created)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		created = now.UnixMilli()
	case err != nil:
		return d, fmt.Errorf("sqlite: save dictionary %s: %w", d.Key, err)
	}

	search := strings.ToLower(strings.Join([]string{d.Key, d.Name, d.Description}, " "))
	// DO NOTHING rather than a read followed by a write: the conflict is
	// resolved by the database, inside this transaction, so a second client
	// creating the same key loses the race rather than the first client's
	// phrases.
	conflict := "DO UPDATE SET name = excluded.name, description = excluded.description, " +
		"score = excluded.score, search = excluded.search, updated_at = excluded.updated_at"
	if mustBeNew {
		conflict = "DO NOTHING"
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO hotword_dicts (key, name, description, score, search, created_at, updated_at)
VALUES (?,?,?,?,?,?,?)
ON CONFLICT(key) `+conflict,
		d.Key, d.Name, d.Description, d.Score, search, created, now.UnixMilli())
	if err != nil {
		return d, fmt.Errorf("sqlite: save dictionary %s: %w", d.Key, err)
	}
	if mustBeNew {
		n, err := res.RowsAffected()
		if err != nil {
			return d, fmt.Errorf("sqlite: save dictionary %s: %w", d.Key, err)
		}
		if n == 0 {
			return d, core.Errorf(core.CodeDictionaryExists,
				"a dictionary with the key %q already exists", d.Key).WithParam("key")
		}
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM hotword_phrases WHERE dict_key = ?`, d.Key); err != nil {
		return d, fmt.Errorf("sqlite: replace phrases of %s: %w", d.Key, err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO hotword_phrases (dict_key, ord, phrase, phrase_lc) VALUES (?,?,?,?)`)
	if err != nil {
		return d, fmt.Errorf("sqlite: replace phrases of %s: %w", d.Key, err)
	}
	defer stmt.Close()
	for i, phrase := range d.Phrases {
		if _, err := stmt.ExecContext(ctx, d.Key, i, phrase, strings.ToLower(phrase)); err != nil {
			return d, fmt.Errorf("sqlite: insert phrase into %s: %w", d.Key, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return d, fmt.Errorf("sqlite: save dictionary %s: %w", d.Key, err)
	}

	d.CreatedAt = time.UnixMilli(created).UTC()
	d.UpdatedAt = now
	d.PhraseCount = len(d.Phrases)
	return d, nil
}

// Delete removes a dictionary and its phrases.
func (s *DictionaryStore) Delete(ctx context.Context, key string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM hotword_dicts WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("sqlite: delete dictionary %s: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: delete dictionary %s: %w", key, err)
	}
	if n == 0 {
		return core.Errorf(core.CodeDictionaryNotFound, "no such dictionary: %s", key)
	}
	return nil
}

var _ core.Dictionaries = (*DictionaryStore)(nil)
