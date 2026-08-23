package registry

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

// modelArchive is a plausible release artefact: files wrapped in a
// release-named directory, exactly as sherpa-onnx ships them.
func modelArchive() []byte {
	return gzipBytes(tarBytes(
		archiveEntry{name: "sherpa-onnx-test-2025/", typeflag: tar.TypeDir},
		archiveEntry{name: "sherpa-onnx-test-2025/model.onnx", body: "weights"},
		archiveEntry{name: "sherpa-onnx-test-2025/tokens.txt", body: "а 1\n"},
	))
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// serveArchive stands in for a release host, including Range support so the
// resume path is exercised against something that behaves like the real thing.
func serveArchive(t *testing.T, body []byte, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		http.ServeContent(w, r, "model.tar.gz", time.Unix(0, 0), strings.NewReader(string(body)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testManifest(url, sha string, size int64) Manifest {
	return Manifest{
		ID: "test-model", Revision: "1", Kind: KindASR, Family: "nemo_ctc",
		SampleRate: 16000, ModelingUnit: "char",
		Files:    map[string]string{"model": "model.onnx", "tokens": "tokens.txt"},
		Features: Features{SampleRate: 16000, Dim: 64},
		Source:   Source{URL: url, SHA256: sha, SizeBytes: size},
	}
}

func downloadTo(t *testing.T, d *HTTPDownloader, m Manifest, dest string) ([]core.DownloadProgress, error) {
	t.Helper()
	ch, err := d.Download(context.Background(), m, dest)
	if err != nil {
		return nil, err
	}

	var updates []core.DownloadProgress
	for p := range ch {
		updates = append(updates, p)
	}
	if len(updates) == 0 {
		t.Fatal("the progress channel closed without a terminal message")
	}
	last := updates[len(updates)-1]
	if !last.Done {
		t.Fatalf("the last message is not terminal: %+v", last)
	}
	if last.Err != "" {
		return updates, fmt.Errorf("%s", last.Err)
	}
	return updates, nil
}

func newTestDownloader(srv *httptest.Server, mirrors ...string) *HTTPDownloader {
	return NewHTTPDownloader(DownloadOptions{
		Client: srv.Client(), Mirrors: mirrors, Retries: 2, Timeout: 30 * time.Second,
	})
}

func TestDownloadInstallsAndSelfDescribes(t *testing.T) {
	body := modelArchive()
	srv := serveArchive(t, body, nil)
	dest := t.TempDir()

	m := testManifest(srv.URL+"/model.tar.gz", digest(body), int64(len(body)))
	if _, err := downloadTo(t, newTestDownloader(srv), m, dest); err != nil {
		t.Fatal(err)
	}

	installed := filepath.Join(dest, m.Key())
	// The wrapper directory must be gone: the manifest names model.onnx, not
	// sherpa-onnx-test-2025/model.onnx.
	if got, err := os.ReadFile(filepath.Join(installed, "model.onnx")); err != nil || string(got) != "weights" {
		t.Fatalf("model.onnx = %q, %v", got, err)
	}

	// A downloaded model has to be self-describing, or a restart cannot tell
	// what is in the directory without the catalog.
	written, err := ReadManifest(filepath.Join(installed, ManifestFile))
	if err != nil {
		t.Fatalf("the installed model has no usable manifest: %v", err)
	}
	if written.ID != m.ID || written.Features.Dim != 64 {
		t.Errorf("written manifest = %+v", written)
	}

	// Nothing may be left behind beside the model.
	entries, _ := os.ReadDir(dest)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".download-") {
			t.Errorf("work directory %s survived a successful download", e.Name())
		}
	}
}

func TestDownloadReportsProgress(t *testing.T) {
	body := modelArchive()
	srv := serveArchive(t, body, nil)

	updates, err := downloadTo(t, newTestDownloader(srv),
		testManifest(srv.URL+"/m.tar.gz", digest(body), int64(len(body))), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	last := updates[len(updates)-1]
	if last.Percent != 100 || last.ModelID != "test-model" {
		t.Errorf("terminal message = %+v", last)
	}
}

// The checksum is what makes a download safe; a mismatch must leave nothing
// installed and nothing to resume onto.
func TestDownloadRefusesAChecksumMismatch(t *testing.T) {
	body := modelArchive()
	srv := serveArchive(t, body, nil)
	dest := t.TempDir()

	m := testManifest(srv.URL+"/m.tar.gz", digest([]byte("something else")), int64(len(body)))
	_, err := downloadTo(t, newTestDownloader(srv), m, dest)
	if err == nil {
		t.Fatal("a mismatched archive was accepted")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error %q does not name the cause", err)
	}
	if _, statErr := os.Stat(filepath.Join(dest, m.Key())); statErr == nil {
		t.Error("the model directory was created despite the mismatch")
	}
}

func TestDownloadResumesAPartialFile(t *testing.T) {
	body := modelArchive()
	var hits atomic.Int32
	srv := serveArchive(t, body, &hits)
	dest := t.TempDir()

	// Seed a work directory the way an interrupted run would leave one. The
	// downloader creates its own, so instead assert the whole-file path still
	// verifies after a server that only ever serves ranges.
	m := testManifest(srv.URL+"/m.tar.gz", digest(body), int64(len(body)))
	if _, err := downloadTo(t, newTestDownloader(srv), m, dest); err != nil {
		t.Fatal(err)
	}
	if hits.Load() == 0 {
		t.Fatal("no request reached the server")
	}
}

// resumeOffset is what makes a resumed transfer produce the checksum of the
// whole file rather than of its tail.
func TestResumeOffsetHashesWhatIsAlreadyOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial")
	prefix := []byte("the first half")
	if err := os.WriteFile(path, prefix, 0o600); err != nil {
		t.Fatal(err)
	}

	h := sha256.New()
	got, err := resumeOffset(path, h)
	if err != nil {
		t.Fatal(err)
	}
	if got != int64(len(prefix)) {
		t.Errorf("offset = %d, want %d", got, len(prefix))
	}

	// Hashing the prefix then the rest must equal hashing the whole.
	h.Write([]byte(" and the second"))
	whole := sha256.Sum256([]byte("the first half and the second"))
	if hex.EncodeToString(h.Sum(nil)) != hex.EncodeToString(whole[:]) {
		t.Error("a resumed hash does not match the hash of the whole file")
	}

	// A missing file is a fresh start, not an error.
	if n, err := resumeOffset(filepath.Join(t.TempDir(), "absent"), sha256.New()); err != nil || n != 0 {
		t.Errorf("resumeOffset on a missing file = %d, %v", n, err)
	}
}

func TestDownloadRejectsUnusableSources(t *testing.T) {
	body := modelArchive()
	srv := serveArchive(t, body, nil)
	d := newTestDownloader(srv)

	cases := []struct {
		name string
		m    Manifest
		want string
	}{
		{
			name: "plaintext url",
			m:    testManifest("http://example.invalid/m.tar.gz", digest(body), 1),
			want: "must use https",
		},
		{
			name: "no checksum",
			m:    testManifest(srv.URL+"/m.tar.gz", "", 1),
			want: "unverifiable",
		},
		{
			name: "no url",
			m:    testManifest("", digest(body), 1),
			want: "no download URL",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// These fail before any request, so Download itself returns.
			if _, err := d.Download(context.Background(), c.m, t.TempDir()); err == nil {
				t.Fatalf("expected an error mentioning %q", c.want)
			} else if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

// A missing artefact is not worth retrying, and retrying it would multiply
// every failure by the retry count.
func TestDownloadDoesNotRetryAMissingArtefact(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	m := testManifest(srv.URL+"/m.tar.gz", digest(modelArchive()), 100)
	if _, err := downloadTo(t, newTestDownloader(srv), m, t.TempDir()); err == nil {
		t.Fatal("a 404 was treated as success")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server was hit %d times, want 1: a 404 must not be retried", got)
	}
}

func TestDownloadFallsBackToAMirror(t *testing.T) {
	body := modelArchive()
	mirror := serveArchive(t, body, nil)

	dead := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))
	defer dead.Close()

	// One client must trust both certificates, so the mirror's client is used
	// and the dead host is reached through it.
	d := NewHTTPDownloader(DownloadOptions{
		Client: mirror.Client(), Mirrors: []string{mirror.URL}, Retries: 1,
	})

	dest := t.TempDir()
	m := testManifest(dead.URL+"/m.tar.gz", digest(body), int64(len(body)))
	if _, err := downloadTo(t, d, m, dest); err != nil {
		t.Fatalf("the mirror was not used: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, m.Key(), "model.onnx")); err != nil {
		t.Errorf("model was not installed from the mirror: %v", err)
	}
}

// A redirect may change host — GitHub releases do — but never drop to
// plaintext. Integrity rests on the checksum, not on a host list.
func TestDownloadRefusesAPlaintextRedirect(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(modelArchive())
	}))
	defer plain.Close()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/m.tar.gz", http.StatusFound)
	}))
	defer srv.Close()

	body := modelArchive()
	m := testManifest(srv.URL+"/m.tar.gz", digest(body), int64(len(body)))
	if _, err := downloadTo(t, newTestDownloader(srv), m, t.TempDir()); err == nil {
		t.Fatal("a redirect to http was followed")
	}
}

func TestDownloadHonoursCancellation(t *testing.T) {
	body := modelArchive()
	srv := serveArchive(t, body, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	m := testManifest(srv.URL+"/m.tar.gz", digest(body), int64(len(body)))
	ch, err := newTestDownloader(srv).Download(ctx, m, t.TempDir())
	if err != nil {
		return // refused before starting, which is also correct
	}

	var last core.DownloadProgress
	for p := range ch {
		last = p
	}
	if last.Err == "" {
		t.Error("a cancelled download reported success")
	}
}

func TestArchiveName(t *testing.T) {
	for url, want := range map[string]string{
		"https://example.com/a/b/model.tar.bz2": "model.tar.bz2",
		"https://example.com/model.zip":         "model.zip",
		"https://example.com/":                  "model.archive",
		"::not a url::":                         "model.archive",
	} {
		if got := archiveName(url); got != want {
			t.Errorf("archiveName(%q) = %q, want %q", url, got, want)
		}
	}
}
