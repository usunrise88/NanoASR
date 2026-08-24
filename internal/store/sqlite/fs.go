package sqlite

import (
	"fmt"
	"os"
)

// ensureDir creates the directory holding the database. A fresh install points
// db_path at a directory that does not exist yet, and failing there with a bare
// "unable to open database file" tells the operator nothing.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("sqlite: create %s: %w", dir, err)
	}
	return nil
}
