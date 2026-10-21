package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// TestManagedBinary runs a built binary the way an application does: a child
// process with pipes on its standard streams. CI points it at the archive it
// has just built on each platform, which is where the native libraries are
// found the way they will be on a user's machine.
func TestManagedBinary(t *testing.T) {
	bin := os.Getenv("NANOASR_MANAGED_BINARY")
	if bin == "" {
		t.Skip("NANOASR_MANAGED_BINARY names no binary to run")
	}
	cmd := exec.Command(bin, "managed", "-data-dir", t.TempDir(), "-parent-pid", strconv.Itoa(os.Getpid()))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	lines := make(chan map[string]any, 8)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var ev map[string]any
			if json.Unmarshal(sc.Bytes(), &ev) == nil {
				lines <- ev
			}
		}
	}()
	next := func() map[string]any {
		select {
		case ev := <-lines:
			return ev
		case <-time.After(60 * time.Second):
			t.Fatal("no event within 60s")
		}
		return nil
	}

	ready := next()
	if ready["event"] != "ready" {
		t.Fatalf("first event: %v", ready)
	}
	req, _ := http.NewRequest(http.MethodGet, ready["url"].(string)+"/api/v1/managed", nil)
	req.Header.Set("Authorization", "Bearer "+ready["token"].(string))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}

	start := time.Now()
	_ = stdin.Close()
	if ev := next(); ev["event"] != "stopping" {
		t.Fatalf("after closing stdin: %v", ev)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exit: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("still running 30s after stdin closed")
	}
	t.Logf("stopped %v after stdin closed", time.Since(start).Round(time.Millisecond))
}
