package adapter

import (
	"io"
	"os"
	"path/filepath"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/spool"
)

// Detach moves the multipart spool to dir under the given name and hands
// ownership to the caller.
//
// Ownership has to be taken from two places, not one. Beyond UploadSource.Close,
// net/http itself calls Request.MultipartForm.RemoveAll when the handler
// returns. Renaming first is what makes that safe: the old path no longer
// exists, so net/http's cleanup is a no-op instead of a deletion. The copying
// fallback is safe for the same reason in reverse — what net/http removes is the
// original, and the job holds its own copy.
func (s *UploadSource) Detach(dir, name string) (*spool.FileSource, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, core.Errorf(core.CodeInternal, "cannot prepare the spool directory").WithCause(err)
	}
	dst := filepath.Join(dir, name)

	if err := s.moveTo(dst); err != nil {
		return nil, err
	}

	out := spool.NewFileSource(dst, s.Filename(), s.Size())
	// The spool is gone from net/http's point of view; make sure a later
	// Close cannot reach for it.
	s.form = nil
	return out, nil
}

// moveTo renames the spool when it can and copies when it cannot: the temp
// directory is frequently on a different filesystem from the one net/http
// spooled into, and a small upload never touched the disk at all.
func (s *UploadSource) moveTo(dst string) error {
	if src := s.spoolPath(); src != "" {
		if err := os.Rename(src, dst); err == nil {
			return nil
		}
	}

	in, err := s.Open()
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return core.Errorf(core.CodeInternal, "cannot spool the upload").WithCause(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return core.Errorf(core.CodeInternal, "cannot spool the upload").WithCause(err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return core.Errorf(core.CodeInternal, "cannot spool the upload").WithCause(err)
	}
	return nil
}

// spoolPath is the temp file net/http created, or "" when the part was small
// enough to stay in memory. multipart.FileHeader keeps the name in an
// unexported field, so it is recovered from the reader's concrete type rather
// than guessed.
func (s *UploadSource) spoolPath() string {
	f, err := s.header.Open()
	if err != nil {
		return ""
	}
	defer f.Close()

	osFile, ok := f.(*os.File)
	if !ok {
		return ""
	}
	return osFile.Name()
}
