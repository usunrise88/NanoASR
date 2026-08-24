package adapter

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/usunrise88/nanoasr/internal/spool"
)

// uploadRequest builds a multipart request carrying n bytes of payload.
//
// maxMemory below the payload size is what forces net/http to spool to disk,
// which is the case Detach exists for.
func uploadRequest(t *testing.T, payload []byte) *http.Request {
	t.Helper()

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "call.mp3")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(payload); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", &body)
	r.Header.Set("Content-Type", w.FormDataContentType())
	return r
}

func TestDetachedAudioSurvivesTheRequestCleanup(t *testing.T) {
	payload := bytes.Repeat([]byte{0x42}, 4096)
	r := uploadRequest(t, payload)

	src, err := NewUploadSource(r, "file", 64) // spools to disk
	if err != nil {
		t.Fatalf("NewUploadSource: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "spool")
	file, err := src.Detach(dir, spool.Name("job_1"))
	if err != nil {
		t.Fatalf("Detach: %v", err)
	}

	// Both cleanups that would otherwise reach the upload: the handler's own
	// defer, and the one net/http runs after the handler returns.
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if r.MultipartForm != nil {
		_ = r.MultipartForm.RemoveAll()
	}

	got, err := file.Open()
	if err != nil {
		t.Fatalf("the job's audio did not survive the request: %v", err)
	}
	defer got.Close()

	content, err := io.ReadAll(got)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(content, payload) {
		t.Errorf("read %d bytes, want %d", len(content), len(payload))
	}
	if file.Size() != int64(len(payload)) || file.Filename() != "call.mp3" {
		t.Errorf("metadata = %q/%d", file.Filename(), file.Size())
	}
}

// A part small enough to stay in RAM never had a temp file to rename, so Detach
// has to fall back to writing one.
func TestDetachSpoolsAnInMemoryUpload(t *testing.T) {
	payload := []byte("RIFF....WAVEfmt ")
	r := uploadRequest(t, payload)

	src, err := NewUploadSource(r, "file", 1<<20) // stays in memory
	if err != nil {
		t.Fatalf("NewUploadSource: %v", err)
	}

	dir := t.TempDir()
	file, err := src.Detach(dir, spool.Name("job_2"))
	if err != nil {
		t.Fatalf("Detach: %v", err)
	}
	_ = src.Close()

	content, err := os.ReadFile(file.Path())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(content, payload) {
		t.Errorf("content = %q, want %q", content, payload)
	}
	if info, err := os.Stat(file.Path()); err == nil && info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600: uploaded audio is not world-readable", info.Mode().Perm())
	}
}

func TestDetachReportsAnUnusableDirectory(t *testing.T) {
	r := uploadRequest(t, []byte("x"))
	src, err := NewUploadSource(r, "file", 1<<20)
	if err != nil {
		t.Fatalf("NewUploadSource: %v", err)
	}

	// A file where the directory should be.
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := src.Detach(blocked, spool.Name("job_3")); err == nil {
		t.Fatal("Detach into a file-as-directory succeeded")
	}
}

func TestUploadSourceStillCleansUpWhenNotDetached(t *testing.T) {
	r := uploadRequest(t, bytes.Repeat([]byte{1}, 4096))
	src, err := NewUploadSource(r, "file", 64)
	if err != nil {
		t.Fatalf("NewUploadSource: %v", err)
	}
	path := src.spoolPath()
	if path == "" {
		t.Fatal("expected the upload to spool to disk")
	}
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the synchronous path leaked its spool: %v", err)
	}
}
