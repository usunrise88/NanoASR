package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A model's publisher and its exporter are not always the same people: the
// archive carries the weights, and the file that makes them biasable is
// published elsewhere. An entry may name it, and it arrives with the model.
func TestExtraFilesArriveWithTheArchive(t *testing.T) {
	vocab := []byte("▁ро -3.5\nма -2.25\n")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/bpe.model") {
			_, _ = w.Write(vocab)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dir := t.TempDir()
	m := manifestWithExtra(srv.URL+"/bpe.model", sum(vocab), int64(len(vocab)))

	d := newTestDownloader(srv)
	// The archive side is covered by the download tests; this exercises the
	// file beside it directly, which is what the new code path is.
	if err := d.fetchExtras(context.Background(), m, dir, nil); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "bpe.model"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(vocab) {
		t.Errorf("file = %q, want the bytes the server sent", got)
	}

	// Readable by whoever runs the server, not only by whoever installed it:
	// an installer that pulls as one user and a service that runs as another
	// is the normal arrangement, and the archive's own files land 0644.
	info, err := os.Stat(filepath.Join(dir, "bpe.model"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o644 {
		t.Errorf("mode = %o, want 0644 like the files the archive brings", mode)
	}
}

// The checksum is the whole reason a catalog entry may point at a host the
// archive did not come from.
func TestAnExtraFileWithTheWrongBytesIsRefused(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not the vocabulary"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	m := manifestWithExtra(srv.URL+"/bpe.model", sum([]byte("the vocabulary")), 14)

	d := newTestDownloader(srv)
	err := d.fetchExtras(context.Background(), m, dir, nil)
	if err == nil {
		t.Fatal("a file whose checksum does not match was installed")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("error = %v, want it to name the checksum", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bpe.model")); err == nil {
		t.Error("the rejected bytes were left on disk")
	}
}

// Whichever file won would be a silent decision about which bytes the model
// loads from.
func TestAnExtraFileMayNotShadowTheArchive(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bpe.model"), []byte("from the archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := manifestWithExtra("https://example.test/bpe.model", sum([]byte("x")), 1)

	d := NewHTTPDownloader(DownloadOptions{Limits: DefaultUnpackLimits()})
	err := d.fetchExtras(context.Background(), m, dir, nil)
	if err == nil {
		t.Fatal("an extra file overwrote one the archive brought")
	}
	if !strings.Contains(err.Error(), "already contains") {
		t.Errorf("error = %v", err)
	}
}

// A catalog mirror must not be a way to write outside the models directory.
func TestAnExtraFileNameMustBeABareName(t *testing.T) {
	for _, name := range []string{"../escape", "a/b", "", ".", ".."} {
		m := manifestWithExtra("https://example.test/x", sum([]byte("x")), 1)
		m.Source.Extra[0].Name = name
		if err := m.Validate(); err == nil {
			t.Errorf("name %q was accepted", name)
		}
	}
}

func TestAnExtraFileNeedsAChecksum(t *testing.T) {
	m := manifestWithExtra("https://example.test/x", "", 1)
	if err := m.Validate(); err == nil {
		t.Fatal("an unverifiable download was accepted")
	}
	m = manifestWithExtra("", sum([]byte("x")), 1)
	if err := m.Validate(); err == nil {
		t.Fatal("an extra file with no url was accepted")
	}
}

// The repair: a model installed before the entry named the file is complete as
// far as its weights go, and re-downloading 162 MB to add 250 KB would be a
// poor trade.
func TestCompleteAddsAFileToAnInstalledModel(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	m := manifestWithExtra("https://example.test/bpe.model", sum([]byte("x")), 1)
	m.Files = map[string]string{"model": "model.onnx", "tokens": "tokens.txt"}

	// Installed by an older build: the weights, and a manifest that knows
	// nothing about a vocabulary.
	dir := filepath.Join(root, m.Key())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"model.onnx", "tokens.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	installed := m
	installed.Files = map[string]string{"model": "model.onnx", "tokens": "tokens.txt"}
	installed.Source.Extra = nil
	if err := writeManifest(filepath.Join(dir, ManifestFile), installed); err != nil {
		t.Fatal(err)
	}

	// The catalog entry it would have been installed from today.
	current := m
	current.Files = map[string]string{"model": "model.onnx", "tokens": "tokens.txt", "bpe_vocab": "bpe.model"}

	d := newFakeDownloader()
	r := remoteOver(t, root, d, current)

	added, err := r.Complete(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 || added[0] != "bpe.model" {
		t.Fatalf("added = %v, want the vocabulary", added)
	}
	if _, err := os.Stat(filepath.Join(dir, "bpe.model")); err != nil {
		t.Errorf("the file was reported as added and is not there: %v", err)
	}

	// The manifest on disk now points at it, and the registry answers with the
	// manifest that does — without that, the file is on disk and invisible.
	got, err := r.Resolve(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Files["bpe_vocab"] != "bpe.model" {
		t.Errorf("resolved files = %v, want bpe_vocab wired", got.Files)
	}

	// Idempotent: running it again downloads nothing and changes nothing.
	before := d.extras.Load()
	again, err := r.Complete(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 || d.extras.Load() != before {
		t.Errorf("a second completion did %v and %d more downloads", again, d.extras.Load()-before)
	}
}

// The manifest on disk may carry an operator's own edits — the documentation
// told people to add exactly this file by hand before the catalog could — so it
// is edited rather than replaced.
func TestCompleteKeepsWhatIsAlreadyInTheManifest(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	m := manifestWithExtra("https://example.test/bpe.model", sum([]byte("x")), 1)
	m.Files = map[string]string{"model": "model.onnx", "tokens": "tokens.txt"}

	dir := filepath.Join(root, m.Key())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"model.onnx", "tokens.txt", "bpe.model"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	installed := m
	installed.Source.Extra = nil
	installed.Notes = "hand-tuned on site"
	installed.Runtime.DecodingMethod = "modified_beam_search"
	installed.Files = map[string]string{"model": "model.onnx", "tokens": "tokens.txt", "bpe_vocab": "bpe.model"}
	if err := writeManifest(filepath.Join(dir, ManifestFile), installed); err != nil {
		t.Fatal(err)
	}

	current := m
	current.Files = map[string]string{"model": "model.onnx", "tokens": "tokens.txt", "bpe_vocab": "bpe.model"}

	d := newFakeDownloader()
	r := remoteOver(t, root, d, current)

	added, err := r.Complete(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 {
		t.Errorf("added = %v, want nothing: the file and the manifest entry are both there", added)
	}
	if d.extras.Load() != 0 {
		t.Error("a file that was already on disk was downloaded again")
	}

	back, err := ReadManifest(filepath.Join(dir, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if back.Notes != "hand-tuned on site" || back.Runtime.DecodingMethod != "modified_beam_search" {
		t.Errorf("manifest = %+v, want the operator's edits kept", back)
	}
}

// remoteOver builds a registry over a models directory that already holds
// something, with a catalog of exactly the entries given.
func remoteOver(t *testing.T, root string, d Downloader, catalog ...Manifest) *Remote {
	t.Helper()
	local, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	y, err := yaml.Marshal(map[string]any{"version": 1, "models": catalog})
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRemote(local, d, RemoteOptions{AllowDownload: true, CatalogYAML: y})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func manifestWithExtra(url, sha string, size int64) Manifest {
	return Manifest{
		ID: "test-model", Revision: "1", Kind: KindASR, Family: "transducer",
		SampleRate: 16000, ModelingUnit: "bpe",
		Files:    map[string]string{"model": "model.onnx", "tokens": "tokens.txt"},
		Features: Features{SampleRate: 16000, Dim: 80},
		Source: Source{
			URL: "https://example.test/model.tar.bz2", SHA256: sum([]byte("archive")),
			Extra: []ExtraFile{{Name: "bpe.model", URL: url, SHA256: sha, SizeBytes: size}},
		},
	}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// A catalog revision may add a file role whose file ships inside a newer
// archive and has no source.extra to fetch it with. Writing that name into the
// manifest of an older installation would turn a working model into one that
// cannot load — persistently, because the manifest is on disk.
func TestCompleteDoesNotPointTheManifestAtAFileThatIsNotThere(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	m := manifestWithExtra("https://example.test/bpe.model", sum([]byte("x")), 1)
	m.Files = map[string]string{"model": "model.onnx", "tokens": "tokens.txt"}

	dir := filepath.Join(root, m.Key())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"model.onnx", "tokens.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	installed := m
	installed.Source.Extra = nil
	if err := writeManifest(filepath.Join(dir, ManifestFile), installed); err != nil {
		t.Fatal(err)
	}

	// The catalog today: the vocabulary, which is fetched, and a second file
	// that is only inside a newer archive.
	current := m
	current.Files = map[string]string{
		"model": "model.onnx", "tokens": "tokens.txt",
		"bpe_vocab": "bpe.model", "lm": "lm.onnx",
	}

	r := remoteOver(t, root, newFakeDownloader(), current)
	added, err := r.Complete(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 || added[0] != "bpe.model" {
		t.Fatalf("added = %v, want only the file that was fetched", added)
	}

	back, err := ReadManifest(filepath.Join(dir, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if back.Files["lm"] != "" {
		t.Errorf("manifest points at %q, which was never downloaded", back.Files["lm"])
	}
	if back.Files["bpe_vocab"] != "bpe.model" {
		t.Errorf("manifest files = %v, want the fetched file wired", back.Files)
	}
}
