package adapter

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"

	"github.com/usunrise88/nanoasr/internal/core"
)

// UploadSource turns a multipart file part into a core.AudioSource.
//
// It deliberately does not copy the upload anywhere: net/http already spools a
// large part to a temp file, and FileHeader.Open reopens it, which satisfies
// the re-openable contract without a second copy of a hundred-megabyte file.
// Close removes whatever the spool left behind.
type UploadSource struct {
	header *multipart.FileHeader
	form   *multipart.Form
}

// NewUploadSource wraps the named form file. maxMemory bounds what stays in
// RAM; anything larger spools to disk under the process temp directory.
func NewUploadSource(r *http.Request, field string, maxMemory int64) (*UploadSource, error) {
	if err := r.ParseMultipartForm(maxMemory); err != nil {
		return nil, uploadError(err)
	}

	_, header, err := r.FormFile(field)
	if err != nil {
		if errors.Is(err, http.ErrMissingFile) {
			return nil, core.Errorf(core.CodeInvalidRequest,
				"no %q part in the request", field).WithParam(field)
		}
		return nil, uploadError(err)
	}
	return &UploadSource{header: header, form: r.MultipartForm}, nil
}

func (s *UploadSource) Open() (io.ReadCloser, error) {
	f, err := s.header.Open()
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "cannot reopen the upload").WithCause(err)
	}
	return f, nil
}

// Filename is the client-supplied name, reduced to its base component.
//
// It is reported and logged, never used to build a path: the client chooses it
// and "../../etc/passwd" is a perfectly valid multipart filename.
func (s *UploadSource) Filename() string {
	return filepath.Base(filepath.Clean("/" + s.header.Filename))
}

func (s *UploadSource) Size() int64 { return s.header.Size }

func (s *UploadSource) Close() error {
	if s.form != nil {
		return s.form.RemoveAll()
	}
	return nil
}

func uploadError(err error) error {
	// http.MaxBytesReader is installed by middleware, so an oversized upload
	// surfaces here rather than as a generic parse failure.
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return core.Errorf(core.CodeFileTooLarge,
			"upload exceeds the %d byte limit", maxErr.Limit)
	}
	return core.Errorf(core.CodeInvalidRequest, "malformed multipart request").WithCause(err)
}

var _ core.AudioSource = (*UploadSource)(nil)
