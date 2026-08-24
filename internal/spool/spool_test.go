package spool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/usunrise88/nanoasr/internal/core"
)

func TestNameRoundTrips(t *testing.T) {
	id, ok := JobID(Name("job_abc123"))
	if !ok || id != "job_abc123" {
		t.Fatalf("JobID(Name(x)) = %q, %v", id, ok)
	}
}

func TestJobIDIgnoresForeignFiles(t *testing.T) {
	for _, name := range []string{"", "job-.audio", "ffmpeg-tmp.wav", "job-x.wav", "nanoasr.db"} {
		if _, ok := JobID(name); ok {
			t.Errorf("JobID(%q) claimed a file that is not ours", name)
		}
	}
}

func TestReserveRefusesToExceedTheBudget(t *testing.T) {
	s := New(t.TempDir(), 100)

	if err := s.Reserve("a", 60); err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	err := s.Reserve("b", 60)
	if core.AsError(err).Code != core.CodeQueueFull {
		t.Fatalf("second Reserve = %v, want queue_full", err)
	}
	if s.Used() != 60 {
		t.Errorf("used = %d, want 60 — the refused reservation must not count", s.Used())
	}

	// Once the first job finishes, the second fits.
	s.Release("a")
	if err := s.Reserve("b", 60); err != nil {
		t.Fatalf("Reserve after release: %v", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	s := New(t.TempDir(), 100)
	if err := s.Reserve("a", 40); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	s.Release("a")
	s.Release("a")
	s.Release("never-reserved")
	if s.Used() != 0 {
		t.Fatalf("used = %d, want 0", s.Used())
	}
}

func TestZeroBudgetMeansUnlimited(t *testing.T) {
	s := New(t.TempDir(), 0)
	if err := s.Reserve("a", 1<<40); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
}

func TestRemoveDeletesTheAudioAndFreesTheBytes(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, 1000)

	write(t, s.Path("a"), 100)
	if err := s.Reserve("a", 100); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	s.Remove("a")
	if _, err := os.Stat(s.Path("a")); !os.IsNotExist(err) {
		t.Errorf("audio survived Remove: %v", err)
	}
	if s.Used() != 0 {
		t.Errorf("used = %d after Remove, want 0", s.Used())
	}
	s.Remove("a") // must not panic or complain about a file that is already gone
}

func TestSweepKeepsResumableAudioAndDeletesOrphans(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, 1000)

	write(t, s.Path("alive"), 120)
	write(t, s.Path("orphan"), 200)
	write(t, filepath.Join(dir, "ffmpeg-scratch.wav"), 10)

	removed, err := s.Sweep(map[string]int64{"alive": 120})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed %d files, want 1", removed)
	}
	if _, err := os.Stat(s.Path("alive")); err != nil {
		t.Errorf("Sweep deleted resumable audio: %v", err)
	}
	if _, err := os.Stat(s.Path("orphan")); !os.IsNotExist(err) {
		t.Error("orphaned audio survived Sweep")
	}
	// A file the server did not create is not the server's to delete.
	if _, err := os.Stat(filepath.Join(dir, "ffmpeg-scratch.wav")); err != nil {
		t.Errorf("Sweep deleted a foreign file: %v", err)
	}
	// Resumed work has to keep counting against the budget, or the first
	// restart quietly doubles what the server is willing to hold.
	if s.Used() != 120 {
		t.Errorf("used = %d after Sweep, want 120", s.Used())
	}
}

func TestSweepToleratesAMissingDirectory(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "not-created-yet"), 0)
	if _, err := s.Sweep(nil); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
}

func TestFileSourceReopensAndDoesNotDeleteOnClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, Name("a"))
	write(t, path, 8)

	src := NewFileSource(path, "call.mp3", 8)
	if src.Filename() != "call.mp3" || src.Size() != 8 {
		t.Errorf("source = %q/%d", src.Filename(), src.Size())
	}

	for range 2 { // re-openable is part of the AudioSource contract
		f, err := src.Open()
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		f.Close()
	}
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("Close deleted the job's audio: %v", err)
	}
}

func TestFileSourceReportsMissingAudioAsInternal(t *testing.T) {
	src := NewFileSource(filepath.Join(t.TempDir(), "gone"), "x", 0)
	_, err := src.Open()
	if core.AsError(err).Code != core.CodeInternal {
		t.Fatalf("Open = %v, want internal", err)
	}
}

func write(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, n), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
