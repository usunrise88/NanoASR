package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// instance is one `nanoasr managed` run in this process, with its standard
// streams as pipes, the way an application starts it.
type instance struct {
	stdin  *io.PipeWriter
	events chan map[string]any
	done   chan error
	ready  map[string]any
}

func startManaged(t *testing.T, dir string) *instance {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	inst := &instance{stdin: inW, events: make(chan map[string]any, 16), done: make(chan error, 1)}
	go func() {
		err := managedCommand(context.Background(), []string{"-data-dir", dir, "-low-priority=false"}, inR, outW)
		_ = outW.Close()
		inst.done <- err
	}()
	go func() {
		defer close(inst.events)
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			var ev map[string]any
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				t.Errorf("not a JSON line on stdout: %q", sc.Text())
				continue
			}
			inst.events <- ev
		}
	}()
	t.Cleanup(func() { _ = inW.Close() })
	return inst
}

func (inst *instance) next(t *testing.T) map[string]any {
	t.Helper()
	select {
	case ev, ok := <-inst.events:
		if !ok {
			t.Fatal("stdout closed")
		}
		return ev
	case <-time.After(30 * time.Second):
		t.Fatal("no event within 30s")
	}
	return nil
}

func (inst *instance) waitReady(t *testing.T) {
	t.Helper()
	ev := inst.next(t)
	if ev["event"] != "ready" {
		t.Fatalf("first event %v, want ready", ev)
	}
	inst.ready = ev
}

func (inst *instance) request(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, inst.ready["url"].(string)+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+inst.ready["token"].(string))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (inst *instance) stop(t *testing.T) {
	t.Helper()
	_ = inst.stdin.Close()
	if ev := inst.next(t); ev["event"] != "stopping" || ev["reason"] != "stdin closed" {
		t.Fatalf("after closing stdin: %v", ev)
	}
	select {
	case err := <-inst.done:
		if err != nil {
			t.Fatalf("stopped with %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("did not stop within 30s of stdin closing")
	}
}

// The first start of an instance is its setup; every later one comes up with
// what was chosen then.
func TestManagedSetupIsKeptForTheNextStart(t *testing.T) {
	dir := t.TempDir()

	first := startManaged(t, dir)
	first.waitReady(t)
	if first.ready["configured"] != false || first.ready["api_version"] != float64(1) {
		t.Fatalf("first start: %v", first.ready)
	}

	// The token is the only way in, and only by the loopback name.
	url := first.ready["url"].(string)
	resp, err := http.Get(url + "/api/v1/managed")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without the token: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, url+"/healthz", nil)
	req.Host = fmt.Sprintf("rebound.example:%v", first.ready["port"])
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("a foreign Host: %d", resp.StatusCode)
	}

	code, st := first.request(t, http.MethodPatch, "/api/v1/managed/settings",
		`{"default_model":"gigaam-v3-ctc-punct-ru","diarization":true}`)
	if code != http.StatusOK || st["configured"] != true {
		t.Fatalf("settings: %d %v", code, st)
	}
	missing := fmt.Sprint(st["missing_models"])
	for _, id := range []string{"silero-vad-v5", "gigaam-v3-ctc-punct-ru", "nemotron-3-diarization"} {
		if !strings.Contains(missing, id) {
			t.Errorf("missing_models %s does not name %s", missing, id)
		}
	}
	if code, _ := first.request(t, http.MethodPatch, "/api/v1/managed/settings", `{"isolation":"yes"}`); code != http.StatusBadRequest {
		t.Errorf("a malformed setting: %d", code)
	}

	// One instance per data directory.
	second := startManaged(t, dir)
	if ev := second.next(t); ev["event"] != "fatal" || ev["code"] != "data_dir_locked" {
		t.Fatalf("a second instance on the same directory: %v", ev)
	}
	if err := <-second.done; err == nil {
		t.Fatal("the second instance did not fail")
	}

	first.stop(t)

	again := startManaged(t, dir)
	again.waitReady(t)
	if again.ready["configured"] != true || again.ready["token"] == first.ready["token"] {
		t.Fatalf("second start: %v", again.ready)
	}
	_, st = again.request(t, http.MethodGet, "/api/v1/managed", "")
	settings := st["settings"].(map[string]any)
	if settings["default_model"] != "gigaam-v3-ctc-punct-ru" || settings["diarization"] != true {
		t.Fatalf("the settings did not survive the restart: %v", settings)
	}
	again.stop(t)
}

// A start that fails says why, once, and nothing after: no stop was asked for.
func TestManagedFailureIsOneFatalEvent(t *testing.T) {
	dir := t.TempDir()
	// A database that cannot be opened fails the server after the watchers
	// are running, which is where a false "stopping" could come from.
	if err := os.MkdirAll(filepath.Join(dir, "nanoasr.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	inst := startManaged(t, dir)
	if ev := inst.next(t); ev["event"] != "fatal" || ev["code"] != "server_failed" {
		t.Fatalf("first event: %v", ev)
	}
	if err := <-inst.done; err == nil {
		t.Fatal("the start did not fail")
	}
	for ev := range inst.events {
		t.Errorf("an event after the fatal one: %v", ev)
	}
}
