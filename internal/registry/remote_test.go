package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

// fakeDownloader installs a working model directory without touching the
// network, and counts how many times it was asked to.
type fakeDownloader struct {
	calls   atomic.Int32
	started chan struct{} // closed on the first call
	release chan struct{} // downloads block until this is closed
	fail    string
	once    sync.Once
}

func newFakeDownloader() *fakeDownloader {
	return &fakeDownloader{started: make(chan struct{}), release: make(chan struct{})}
}

func (f *fakeDownloader) Download(ctx context.Context, m Manifest, destDir string) (<-chan core.DownloadProgress, error) {
	f.calls.Add(1)
	f.once.Do(func() { close(f.started) })

	ch := make(chan core.DownloadProgress, 8)
	go func() {
		defer close(ch)

		ch <- core.DownloadProgress{ModelID: m.ID, Percent: 50}
		select {
		case <-f.release:
		case <-ctx.Done():
			ch <- core.DownloadProgress{ModelID: m.ID, Done: true, Err: ctx.Err().Error()}
			return
		}

		if f.fail != "" {
			ch <- core.DownloadProgress{ModelID: m.ID, Done: true, Err: f.fail}
			return
		}
		if err := installFakeModel(destDir, m); err != nil {
			ch <- core.DownloadProgress{ModelID: m.ID, Done: true, Err: err.Error()}
			return
		}
		ch <- core.DownloadProgress{ModelID: m.ID, Percent: 100, Done: true}
	}()
	return ch, nil
}

func installFakeModel(destDir string, m Manifest) error {
	dir := filepath.Join(destDir, m.Key())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range m.Files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			return err
		}
	}
	return writeManifest(filepath.Join(dir, ManifestFile), m)
}

func testCatalog(commercial string) []byte {
	return []byte(fmt.Sprintf(`
version: 1
models:
  - id: catalog-model
    revision: "1"
    kind: asr
    family: nemo_ctc
    languages: [ru]
    sample_rate: 16000
    modeling_unit: char
    files:
      model: model.onnx
      tokens: tokens.txt
    features:
      sample_rate: 16000
      dim: 64
    source:
      url: https://example.invalid/model.tar.gz
      sha256: %s
      size_bytes: 1024
    commercial_use: %s
`, strings.Repeat("a", 64), commercial))
}

func newRemote(t *testing.T, d Downloader, opt RemoteOptions) (*Remote, string) {
	t.Helper()
	root := t.TempDir()

	local, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	if opt.CatalogYAML == nil {
		opt.CatalogYAML = testCatalog(CommercialYes)
	}

	r, err := NewRemote(local, d, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, root
}

// Without a catalog fallback in Resolve, a model that has never been fetched
// cannot even be named, so automatic download is impossible.
func TestResolveFallsBackToTheCatalog(t *testing.T) {
	r, _ := newRemote(t, newFakeDownloader(), RemoteOptions{AllowDownload: true})

	m, err := r.Resolve(context.Background(), "catalog-model")
	if err != nil {
		t.Fatalf("a catalog model could not be resolved: %v", err)
	}
	if m.Family != "nemo_ctc" || m.Features.Dim != 64 {
		t.Errorf("resolved manifest = %+v", m)
	}
}

func TestResolveReportsWhatIsAvailableForAnUnknownModel(t *testing.T) {
	r, _ := newRemote(t, newFakeDownloader(), RemoteOptions{AllowDownload: true})

	_, err := r.Resolve(context.Background(), "no-such-model")
	var e *core.Error
	if !errors.As(err, &e) || e.Code != core.CodeModelNotFound {
		t.Fatalf("got %v, want model_not_found", err)
	}
}

func TestEnsureDownloadsAndInstalls(t *testing.T) {
	d := newFakeDownloader()
	close(d.release)
	r, root := newRemote(t, d, RemoteOptions{AllowDownload: true})

	dir, err := r.Ensure(context.Background(), "catalog-model")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "catalog-model@1"); dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	// The registry must now see it without another download.
	if _, err := r.Ensure(context.Background(), "catalog-model"); err != nil {
		t.Fatal(err)
	}
	if got := d.calls.Load(); got != 1 {
		t.Errorf("downloaded %d times, want 1", got)
	}
}

// A transcription request naming an absent model and an operator pulling the
// same model must not fetch it twice.
func TestConcurrentCallersShareOneDownload(t *testing.T) {
	d := newFakeDownloader()
	r, _ := newRemote(t, d, RemoteOptions{AllowDownload: true})

	const callers = 6
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = r.Ensure(context.Background(), "catalog-model")
		}()
	}

	<-d.started
	time.Sleep(50 * time.Millisecond) // let every caller join the same download
	close(d.release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	if got := d.calls.Load(); got != 1 {
		t.Errorf("downloaded %d times, want 1", got)
	}
}

// Each subscriber gets its own channel, so one consumer cannot eat another's
// events — the flaw that made the old drain-the-channel shape unusable.
func TestFetchFansOutToEverySubscriber(t *testing.T) {
	d := newFakeDownloader()
	r, _ := newRemote(t, d, RemoteOptions{AllowDownload: true})

	first, err := r.Fetch(context.Background(), "catalog-model")
	if err != nil {
		t.Fatal(err)
	}
	<-d.started
	second, err := r.Fetch(context.Background(), "catalog-model")
	if err != nil {
		t.Fatal(err)
	}
	close(d.release)

	for i, ch := range []<-chan core.DownloadProgress{first, second} {
		var last core.DownloadProgress
		count := 0
		for p := range ch {
			last = p
			count++
		}
		if count == 0 {
			t.Errorf("subscriber %d received nothing", i)
		}
		if !last.Done || last.Err != "" {
			t.Errorf("subscriber %d ended with %+v", i, last)
		}
	}
	if got := d.calls.Load(); got != 1 {
		t.Errorf("downloaded %d times, want 1", got)
	}
}

func TestFetchOnAnInstalledModelCompletesImmediately(t *testing.T) {
	d := newFakeDownloader()
	close(d.release)
	r, _ := newRemote(t, d, RemoteOptions{AllowDownload: true})

	if _, err := r.Ensure(context.Background(), "catalog-model"); err != nil {
		t.Fatal(err)
	}

	ch, err := r.Fetch(context.Background(), "catalog-model")
	if err != nil {
		t.Fatal(err)
	}
	var last core.DownloadProgress
	for p := range ch {
		last = p
	}
	if !last.Done || last.Percent != 100 {
		t.Errorf("got %+v, want an immediate completion", last)
	}
	if got := d.calls.Load(); got != 1 {
		t.Errorf("downloaded %d times, want 1 — Fetch re-downloaded an installed model", got)
	}
}

func TestDownloadFailureReachesTheCaller(t *testing.T) {
	d := newFakeDownloader()
	d.fail = "the mirror caught fire"
	close(d.release)
	r, _ := newRemote(t, d, RemoteOptions{AllowDownload: true})

	// This is the case the old interface lost: the failure arrived on a
	// channel the caller had already drained and discarded.
	_, err := r.Ensure(context.Background(), "catalog-model")
	if err == nil {
		t.Fatal("a failed download was reported as success")
	}
	if !strings.Contains(err.Error(), "caught fire") {
		t.Errorf("error %q does not carry the cause", err)
	}
}

func TestEnsureRefusesWhenDownloadingIsDisabled(t *testing.T) {
	d := newFakeDownloader()
	r, _ := newRemote(t, d, RemoteOptions{AllowDownload: false})

	_, err := r.Ensure(context.Background(), "catalog-model")
	if err == nil {
		t.Fatal("a download happened with allow_download off")
	}
	// An air-gapped operator needs to be told what to do, not just refused.
	if !strings.Contains(err.Error(), "models pull") {
		t.Errorf("error %q does not say how to obtain the model", err)
	}
	if got := d.calls.Load(); got != 0 {
		t.Errorf("downloader was called %d times", got)
	}
}

func TestStrictLicenseRefusesWhatItCannotVerify(t *testing.T) {
	for _, commercial := range []string{CommercialNo, CommercialUnknown} {
		t.Run(commercial, func(t *testing.T) {
			r, _ := newRemote(t, newFakeDownloader(), RemoteOptions{
				AllowDownload: true, StrictLicense: true,
				CatalogYAML: testCatalog(commercial),
			})

			_, err := r.Ensure(context.Background(), "catalog-model")
			var e *core.Error
			if !errors.As(err, &e) || e.Code != core.CodeModelForbidden {
				t.Fatalf("got %v, want model_forbidden", err)
			}
		})
	}
}

func TestStrictLicenseAllowsAPositiveDeclaration(t *testing.T) {
	d := newFakeDownloader()
	close(d.release)
	r, _ := newRemote(t, d, RemoteOptions{
		AllowDownload: true, StrictLicense: true, CatalogYAML: testCatalog(CommercialYes),
	})

	if _, err := r.Ensure(context.Background(), "catalog-model"); err != nil {
		t.Fatalf("a model declaring commercial use was refused: %v", err)
	}
}

// A caller that gives up must not cancel the download for everyone else.
func TestAbandoningACallerLeavesTheDownloadRunning(t *testing.T) {
	d := newFakeDownloader()
	r, _ := newRemote(t, d, RemoteOptions{AllowDownload: true})

	patient := make(chan error, 1)
	go func() {
		_, err := r.Ensure(context.Background(), "catalog-model")
		patient <- err
	}()
	<-d.started

	quitter, cancel := context.WithCancel(context.Background())
	go func() { _, _ = r.Ensure(quitter, "catalog-model") }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	close(d.release)
	select {
	case err := <-patient:
		if err != nil {
			t.Fatalf("the remaining caller was affected by an abandoned one: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the download did not finish after one caller gave up")
	}
}

func TestCatalogListsWhatCanBeFetched(t *testing.T) {
	r, _ := newRemote(t, newFakeDownloader(), RemoteOptions{AllowDownload: true})

	entries, err := r.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "catalog-model" {
		t.Fatalf("catalog = %+v", entries)
	}
}
