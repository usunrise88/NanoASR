package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceFlagAcceptsTheWindowsSpelling(t *testing.T) {
	cases := map[string]string{
		"--install":   "install",
		"-install":    "install",
		"--uninstall": "uninstall",
		"--remove":    "uninstall",
		"--status":    "status",
	}
	for arg, want := range cases {
		got, ok := serviceFlag(arg)
		if !ok || got != want {
			t.Errorf("serviceFlag(%q) = %q, %v; want %q, true", arg, got, ok, want)
		}
	}
	// A bare verb is the `nanoasr service install` form and must not be
	// mistaken for a top-level flag, and an unknown flag has to reach the usage
	// message rather than be read as a service verb.
	for _, arg := range []string{"install", "serve", "--help", "-h", "--addr", "--", "-"} {
		if got, ok := serviceFlag(arg); ok {
			t.Errorf("serviceFlag(%q) = %q, true; want false", arg, got)
		}
	}
}

func TestListVerbsReadsAsASentence(t *testing.T) {
	got := listVerbs()
	if !strings.HasPrefix(got, "install, ") || !strings.Contains(got, " or status") {
		t.Errorf("listVerbs() = %q", got)
	}
}

func TestOpenLogWithoutAPathIsStderr(t *testing.T) {
	w, closeLog, err := openLog("")
	if err != nil {
		t.Fatal(err)
	}
	defer closeLog()
	if w != os.Stderr {
		t.Errorf("openLog(\"\") = %v, want os.Stderr", w)
	}
}

// The directory is created because the service writes to a path that nothing
// else has made yet: an install chooses logs\nanoasr.log beside the
// configuration, and a first start would otherwise fail on the directory.
func TestOpenLogCreatesTheDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "nanoasr.log")
	w, closeLog, err := openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("line\n")); err != nil {
		t.Fatal(err)
	}
	closeLog()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "line\n" {
		t.Errorf("log contains %q", b)
	}
}

func TestRollingFileKeepsOneGeneration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nanoasr.log")
	f, err := newRollingFile(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	line := []byte("0123456789\n") // 11 bytes
	for range 5 {
		if _, err := f.Write(line); err != nil {
			t.Fatal(err)
		}
	}

	// Five 11-byte lines against a 32-byte limit roll over on the third and
	// again on the fifth, because a line is never split across generations:
	// the file rotates before the write that would take it past the limit.
	prev, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("no rotated file: %v", err)
	}
	if len(prev) != 2*len(line) {
		t.Errorf("rotated file is %d bytes, want %d", len(prev), 2*len(line))
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cur) != len(line) {
		t.Errorf("current file is %d bytes, want %d", len(cur), len(line))
	}
}

// A restart must not lose the size the file already had, or the log would grow
// past the limit by one generation on every restart.
func TestRollingFileResumesFromTheExistingSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nanoasr.log")
	if err := os.WriteFile(path, make([]byte, 30), 0o640); err != nil {
		t.Fatal(err)
	}
	f, err := newRollingFile(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("the pre-existing 30 bytes were not counted: %v", err)
	}
}
