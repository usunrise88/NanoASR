package registry

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type archiveEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
	size     int64 // overrides len(body) for declared-size tests
}

func tarBytes(entries ...archiveEntry) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		flag := e.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		size := int64(len(e.body))
		if e.size > 0 {
			size = e.size
		}
		_ = tw.WriteHeader(&tar.Header{
			Name: e.name, Mode: 0o644, Size: size,
			Typeflag: flag, Linkname: e.linkname,
		})
		if flag == tar.TypeReg {
			_, _ = io.WriteString(tw, e.body)
		}
	}
	_ = tw.Close()
	return buf.Bytes()
}

func gzipBytes(b []byte) []byte {
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return out.Bytes()
}

func zipBytes(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func writeArchive(t *testing.T, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUnpackHandlesEveryShippedFormat(t *testing.T) {
	files := []archiveEntry{
		{name: "model/", typeflag: tar.TypeDir},
		{name: "model/model.onnx", body: "weights"},
		{name: "model/tokens.txt", body: "а 1\n"},
	}

	cases := []struct {
		name string
		file string
		body []byte
	}{
		{"tar", "m.tar", tarBytes(files...)},
		{"tar.gz", "m.tar.gz", gzipBytes(tarBytes(files...))},
		{"zip", "m.zip", zipBytes(t, archiveEntry{name: "model/model.onnx", body: "weights"},
			archiveEntry{name: "model/tokens.txt", body: "а 1\n"})},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "out")
			if err := Unpack(writeArchive(t, c.file, c.body), dest, DefaultUnpackLimits()); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(dest, "model", "model.onnx"))
			if err != nil || string(got) != "weights" {
				t.Fatalf("model.onnx = %q, %v", got, err)
			}
		})
	}
}

// The catalog ships .tar.bz2, which the standard library can read but not
// write, so the fixture comes from the system tool.
func TestUnpackReadsBzip2(t *testing.T) {
	if _, err := exec.LookPath("bzip2"); err != nil {
		t.Skip("bzip2 is not installed")
	}

	cmd := exec.Command("bzip2", "-c")
	cmd.Stdin = bytes.NewReader(tarBytes(archiveEntry{name: "model.onnx", body: "weights"}))
	compressed, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "out")
	if err := Unpack(writeArchive(t, "m.tar.bz2", compressed), dest, DefaultUnpackLimits()); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "model.onnx")); err != nil || string(got) != "weights" {
		t.Fatalf("model.onnx = %q, %v", got, err)
	}
}

// Every entry in a downloaded archive is untrusted input.
func TestUnpackRefusesHostileArchives(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want string
	}{
		{
			name: "path traversal",
			body: tarBytes(archiveEntry{name: "../escaped.onnx", body: "x"}),
			want: "escapes the destination",
		},
		{
			name: "deep traversal",
			body: tarBytes(archiveEntry{name: "a/b/../../../escaped.onnx", body: "x"}),
			want: "escapes the destination",
		},
		{
			name: "absolute path",
			body: tarBytes(archiveEntry{name: "/etc/passwd", body: "x"}),
			want: "absolute path",
		},
		{
			// A symlink is the classic way out of a directory that path checks
			// alone would have protected.
			name: "symlink",
			body: tarBytes(archiveEntry{name: "link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}),
			want: "unsupported type",
		},
		{
			name: "hard link",
			body: tarBytes(archiveEntry{name: "link", typeflag: tar.TypeLink, linkname: "model.onnx"}),
			want: "unsupported type",
		},
		{
			name: "device node",
			body: tarBytes(archiveEntry{name: "dev", typeflag: tar.TypeChar}),
			want: "unsupported type",
		},
		{
			name: "empty name",
			body: tarBytes(archiveEntry{name: "", body: "x"}),
			want: "no name",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "out")
			err := Unpack(writeArchive(t, "m.tar", c.body), dest, DefaultUnpackLimits())
			if err == nil {
				t.Fatalf("archive was accepted; expected an error mentioning %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
			// Nothing may have landed outside the destination.
			if _, statErr := os.Stat(filepath.Join(filepath.Dir(dest), "escaped.onnx")); statErr == nil {
				t.Error("a rejected entry was written anyway")
			}
		})
	}
}

func TestUnpackEnforcesLimits(t *testing.T) {
	tiny := UnpackLimits{
		MaxArchiveBytes: 1 << 20, MaxUnpackedBytes: 100, MaxFileBytes: 60, MaxFiles: 2,
	}

	cases := []struct {
		name string
		body []byte
		want string
	}{
		{
			name: "too many files",
			body: tarBytes(archiveEntry{name: "a", body: "x"}, archiveEntry{name: "b", body: "x"}, archiveEntry{name: "c", body: "x"}),
			want: "more than 2 files",
		},
		{
			name: "single file too large",
			body: tarBytes(archiveEntry{name: "a", body: strings.Repeat("x", 61)}),
			want: "over the 60 byte limit",
		},
		{
			name: "total too large",
			body: tarBytes(archiveEntry{name: "a", body: strings.Repeat("x", 60)},
				archiveEntry{name: "b", body: strings.Repeat("x", 60)}),
			want: "total unpacked size limit",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "out")
			err := Unpack(writeArchive(t, "m.tar", c.body), dest, tiny)
			if err == nil {
				t.Fatalf("expected an error mentioning %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

// The limits are enforced against the bytes that actually arrive, not against
// the size an archive declares. Tar cannot express that difference — its header
// size frames the stream — but zip can, and a zip bomb is exactly a small
// declared size in front of a large one. This checks the guard itself rather
// than trying to hand-craft a lying archive.
func TestWriteEntryStopsAtTheAllowance(t *testing.T) {
	target := filepath.Join(t.TempDir(), "out.bin")

	written, err := writeEntry(strings.NewReader(strings.Repeat("x", 500)), target, 100)
	if err == nil {
		t.Fatal("writeEntry accepted more bytes than its allowance")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error %q does not mention the limit", err)
	}
	if written <= 100 {
		t.Errorf("written = %d; the guard must notice the overrun, not truncate silently", written)
	}

	// The allowance itself is not an error.
	if _, err := writeEntry(strings.NewReader("short"), target, 100); err != nil {
		t.Errorf("a payload inside the allowance was rejected: %v", err)
	}
	if _, err := writeEntry(strings.NewReader("x"), target, 0); err == nil {
		t.Error("an exhausted allowance should refuse the next entry")
	}
}

func TestSafeJoin(t *testing.T) {
	dest := "/models/x"
	for _, name := range []string{"../a", "a/../../b", "/abs", "..", ""} {
		if got, err := safeJoin(dest, name); err == nil {
			t.Errorf("safeJoin accepted %q → %q", name, got)
		}
	}
	for name, want := range map[string]string{
		"a.onnx":       "/models/x/a.onnx",
		"sub/a.onnx":   "/models/x/sub/a.onnx",
		"./a.onnx":     "/models/x/a.onnx",
		"a/./b/c.onnx": "/models/x/a/b/c.onnx",
	} {
		got, err := safeJoin(dest, name)
		if err != nil || got != want {
			t.Errorf("safeJoin(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
}

// Every sherpa-onnx archive wraps its files in a release-named directory; the
// manifest describes the files without it.
func TestFlattenSingleDirectory(t *testing.T) {
	root := t.TempDir()
	inner := filepath.Join(root, "sherpa-onnx-model-2025")
	if err := os.MkdirAll(filepath.Join(inner, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"model.onnx", "tokens.txt"} {
		if err := os.WriteFile(filepath.Join(inner, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := flattenSingleDirectory(root); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"model.onnx", "tokens.txt", "sub"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("%s was not lifted: %v", name, err)
		}
	}
	if _, err := os.Stat(inner); err == nil {
		t.Error("the wrapper directory survived")
	}
}

func TestFlattenLeavesAlreadyFlatDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "model.onnx"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := flattenSingleDirectory(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "model.onnx")); err != nil {
		t.Errorf("a flat directory was disturbed: %v", err)
	}
}
