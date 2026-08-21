package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/usunrise88/nanoasr/internal/core"
)

func validManifest() Manifest {
	return Manifest{
		ID:           "gigaam-v2-ctc-ru",
		Revision:     "1",
		Family:       "nemo_ctc",
		SampleRate:   16000,
		ModelingUnit: "bpe",
		Files:        map[string]string{"model": "model.onnx", "tokens": "tokens.txt"},
		Features:     Features{SampleRate: 16000, Dim: 64},
	}
}

func TestManifestValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{"valid", func(*Manifest) {}, ""},
		{"bad id", func(m *Manifest) { m.ID = "../etc/passwd" }, "must match"},
		{"empty id", func(m *Manifest) { m.ID = "" }, "must match"},
		{"no family", func(m *Manifest) { m.Family = "" }, "family is required"},
		{"no sample rate", func(m *Manifest) { m.SampleRate = 0 }, "sample_rate is required"},
		{"no files", func(m *Manifest) { m.Files = nil }, "files is empty"},
		{"no feature dim", func(m *Manifest) { m.Features.Dim = 0 }, "features.dim"},
		{
			name:   "downloadable without checksum",
			mutate: func(m *Manifest) { m.Source.URL = "https://example.invalid/model.tar.bz2" },
			want:   "sha256 is required",
		},
		{
			name:   "file escaping the model directory",
			mutate: func(m *Manifest) { m.Files["model"] = "../../etc/passwd" },
			want:   "unusable path",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := validManifest()
			c.mutate(&m)
			err := m.Validate()

			if c.want == "" {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

// Manifests can arrive inside a downloaded archive, so file names are untrusted.
func TestManifestFilePathRejectsEscapes(t *testing.T) {
	m := validManifest()
	for _, name := range []string{"/etc/passwd", "../secrets.onnx", "sub/../../out.onnx", `windows\path.onnx`} {
		m.Files["model"] = name
		if _, err := m.FilePath("/models/x", "model"); err == nil {
			t.Errorf("FilePath accepted %q, want rejection", name)
		}
	}

	m.Files["model"] = "sub/model.int8.onnx"
	got, err := m.FilePath("/models/x", "model")
	if err != nil {
		t.Fatalf("FilePath rejected a legitimate relative path: %v", err)
	}
	if got != "/models/x/sub/model.int8.onnx" {
		t.Errorf("FilePath = %q", got)
	}
}

func TestManifestFeatureSampleRateFallsBackToModelRate(t *testing.T) {
	m := validManifest()
	m.Features.SampleRate = 0
	if got := m.FeatureSampleRate(); got != 16000 {
		t.Errorf("FeatureSampleRate = %d, want the model rate 16000", got)
	}
}

func writeModel(t *testing.T, root, dir, yaml string) string {
	t.Helper()
	full := filepath.Join(root, dir)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, ManifestFile), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return full
}

const modelYAML = `
id: %s
revision: "%s"
family: nemo_ctc
display_name: test
languages: [ru]
sample_rate: 16000
modeling_unit: bpe
files:
  model: model.onnx
  tokens: tokens.txt
features:
  sample_rate: 16000
  dim: 64
`

func TestLocalScan(t *testing.T) {
	root := t.TempDir()
	writeModel(t, root, "a-v1", fmt.Sprintf(modelYAML, "model-a", "1"))
	writeModel(t, root, "a-v2", fmt.Sprintf(modelYAML, "model-a", "2"))
	writeModel(t, root, "b", fmt.Sprintf(modelYAML, "model-b", "1"))
	writeModel(t, root, "broken", "id: broken\nfamily: nemo_ctc\n") // no sample_rate, no files
	if err := os.MkdirAll(filepath.Join(root, "not-a-model"), 0o755); err != nil {
		t.Fatal(err)
	}

	reg, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	models, err := reg.Local(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("found %d models %v, want 3 (two revisions of a, one b)", len(models), models)
	}

	// A bare id must resolve deterministically to the highest revision.
	m, err := reg.Resolve(ctx, "model-a")
	if err != nil {
		t.Fatal(err)
	}
	if m.Revision != "2" {
		t.Errorf("model-a resolved to revision %q, want the highest revision 2", m.Revision)
	}

	// A pinned revision must still be reachable.
	if m, err = reg.Resolve(ctx, "model-a@1"); err != nil || m.Revision != "1" {
		t.Errorf("model-a@1 = %v, %v", m.Revision, err)
	}

	// A directory without a manifest is not a model; a broken one is reported.
	problems := reg.Problems()
	if len(problems) != 1 || !strings.Contains(problems[0], "broken") {
		t.Errorf("Problems() = %v, want exactly the broken manifest", problems)
	}
}

func TestLocalMissingModelNamesWhatIsAvailable(t *testing.T) {
	root := t.TempDir()
	writeModel(t, root, "b", fmt.Sprintf(modelYAML, "model-b", "1"))

	reg, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}

	_, err = reg.Resolve(context.Background(), "does-not-exist")
	var e *core.Error
	if !errors.As(err, &e) || e.Code != core.CodeModelNotFound {
		t.Fatalf("got %v, want model_not_found", err)
	}
	if !strings.Contains(err.Error(), "model-b@1") {
		t.Errorf("error should list what is available, got %q", err)
	}
}

func TestLocalMissingDirectoryIsNotAStartupFailure(t *testing.T) {
	// A fresh install has no models yet. That must surface when a request names
	// a model, not by refusing to boot.
	reg, err := NewLocal(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("NewLocal on a missing directory failed: %v", err)
	}
	models, err := reg.Local(context.Background())
	if err != nil || len(models) != 0 {
		t.Errorf("Local() = %v, %v; want an empty list", models, err)
	}
}

func TestLocalRefreshPicksUpNewModels(t *testing.T) {
	root := t.TempDir()
	reg, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}

	writeModel(t, root, "late", fmt.Sprintf(modelYAML, "model-late", "1"))
	if _, err := reg.Resolve(context.Background(), "model-late"); err == nil {
		t.Fatal("model appeared without a refresh")
	}
	if err := reg.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Resolve(context.Background(), "model-late"); err != nil {
		t.Fatalf("after Refresh: %v", err)
	}
}

func TestReadManifestRejectsUnknownFields(t *testing.T) {
	root := t.TempDir()
	dir := writeModel(t, root, "typo", fmt.Sprintf(modelYAML, "model-typo", "1")+"\nfeature_dim: 80\n")

	// feature_dim instead of features.dim is exactly the typo that would
	// otherwise produce silently wrong transcripts.
	if _, err := ReadManifest(filepath.Join(dir, ManifestFile)); err == nil {
		t.Fatal("unknown manifest field was accepted")
	}
}
