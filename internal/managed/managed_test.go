package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/config"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/registry"
)

func TestSettingsRoundTripAndPatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if _, configured, err := LoadSettings(path); err != nil || configured {
		t.Fatalf("no file: configured=%v err=%v, want an unconfigured instance", configured, err)
	}

	s := Settings{DefaultModel: "a", Diarization: true, Threads: 2}
	if err := SaveSettings(path, s); err != nil {
		t.Fatal(err)
	}
	got, configured, err := LoadSettings(path)
	if err != nil || !configured || got.DefaultModel != "a" || !got.Diarization || got.Threads != 2 {
		t.Fatalf("loaded %+v configured=%v err=%v", got, configured, err)
	}

	// A patch changes what it names and keeps the rest.
	next, err := got.Patch([]byte(`{"realtime_model":"rt","threads":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if next.RealtimeModel != "rt" || next.Threads != 0 || next.DefaultModel != "a" || !next.Diarization {
		t.Fatalf("patched %+v", next)
	}

	_, err = got.Patch([]byte(`{"realtme_model":"rt"}`))
	if core.AsError(err).Code != core.CodeInvalidRequest {
		t.Fatalf("a misspelt field = %v, want invalid_request", err)
	}
}

func catalog(id string) (registry.Manifest, error) {
	switch id {
	case "offline":
		return registry.Manifest{ID: id, Kind: registry.KindASR}, nil
	case "streaming":
		return registry.Manifest{ID: id, Kind: registry.KindASR, Streaming: true}, nil
	case "diar":
		return registry.Manifest{ID: id, Kind: registry.KindDiarization, Family: "sortformer"}, nil
	}
	return registry.Manifest{}, core.Errorf(core.CodeModelNotFound, "model %q is not in the catalog", id)
}

func TestValidateNamesTheField(t *testing.T) {
	for _, tc := range []struct {
		s     Settings
		param string
	}{
		{Settings{DefaultModel: "nope"}, "default_model"},
		{Settings{DefaultModel: "diar"}, "default_model"},
		{Settings{RealtimeModel: "offline"}, "realtime_model"},
		{Settings{Diarization: true, DiarizationModel: "offline"}, "diarization_model"},
		{Settings{Isolation: true}, "isolation"},
		{Settings{Threads: -1}, "threads"},
		{Settings{MaxConcurrent: 100}, "max_concurrent"},
		{Settings{FFmpegPath: "/no/such/ffmpeg"}, "ffmpeg_path"},
		{Settings{Mirrors: []string{"http://plain.example"}}, "mirrors"},
		{Settings{HistoryTTL: "a month"}, "history_ttl"},
		{Settings{LogLevel: "loud"}, "log_level"},
	} {
		err := tc.s.Validate(catalog)
		if e := core.AsError(err); e == nil || e.Param != tc.param {
			t.Errorf("%+v: %v, want a refusal of %s", tc.s, err, tc.param)
		}
	}
	ok := Settings{DefaultModel: "offline", RealtimeModel: "streaming", Diarization: true,
		DiarizationModel: "diar", Isolation: true, HistoryTTL: "720h", LogLevel: "debug"}
	if err := ok.Validate(catalog); err != nil {
		t.Fatalf("valid settings refused: %v", err)
	}
}

func planWith(t *testing.T, s Settings, present ...string) Plan {
	t.Helper()
	p, err := NewPlan(s, Options{
		Paths: PathsFor(t.TempDir()),
		Token: "sk-test-0123456789abcdef",
		Present: func(id string) bool {
			return slices.Contains(present, id)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A model chosen but not downloaded must not keep the instance from
// starting: the application downloads it through this very process.
func TestPlanStartsWithoutTheModels(t *testing.T) {
	s := Settings{DefaultModel: "offline", RealtimeModel: "streaming", Diarization: true}
	p := planWith(t, s)
	cfg := p.Config

	if cfg.VAD.Enabled {
		t.Error("the VAD is enabled without its model")
	}
	if cfg.API.DialectEnabled(config.DialectRealtime) {
		t.Error("realtime is mounted without its model")
	}
	if !cfg.Diarization.Enabled || cfg.ASR.DefaultModel != "offline" {
		t.Error("the lazy features were left out")
	}
	if cfg.Registry.AutoDownload || !cfg.Registry.AllowDownload {
		t.Error("downloads must be explicit and allowed")
	}
	if cfg.Server.Addr != "127.0.0.1:0" || cfg.Auth.Mode != "apikey" || len(cfg.Auth.Keys) != 1 {
		t.Errorf("listener or auth: %q %q %d keys", cfg.Server.Addr, cfg.Auth.Mode, len(cfg.Auth.Keys))
	}
	if cfg.UI.Enabled {
		t.Error("the UI is on without being asked for")
	}

	all := planWith(t, s, "silero-vad-v5", "offline", "streaming", config.Default().Diarization.Model)
	if !all.Config.VAD.Enabled || !all.Config.API.DialectEnabled(config.DialectRealtime) ||
		all.Config.Realtime.Model != "streaming" {
		t.Error("with every model present, the VAD and realtime are not on")
	}
}

// On provider coreml the settings go on naming the models the application
// chose, and the plan runs, and asks for, the CoreML build of each one that
// has a build. One without a build runs as itself, on the CPU.
func TestPlanRunsTheCoreMLBuilds(t *testing.T) {
	s := Settings{DefaultModel: "gigaam-v3-ctc-punct-ru", RealtimeModel: "t-one-ctc-ru",
		Diarization: true, Isolation: true, Provider: config.ProviderCoreML}
	builds := []string{"silero-vad-v5-coreml", "gigaam-v3-ctc-punct-ru-coreml", "t-one-ctc-ru-coreml",
		"nemotron-3-diarization-coreml", "voxlock-int8-coreml", "campplus-sv-zh-en-coreml"}
	p := planWith(t, s, builds...)
	cfg := p.Config
	for _, c := range [][2]string{
		{cfg.VAD.Model, "silero-vad-v5-coreml"},
		{cfg.ASR.DefaultModel, "gigaam-v3-ctc-punct-ru-coreml"},
		{cfg.Realtime.Model, "t-one-ctc-ru-coreml"},
		{cfg.Diarization.Model, "nemotron-3-diarization-coreml"},
		{cfg.Isolation.Model, "voxlock-int8-coreml"},
		{cfg.Isolation.EmbeddingModel, "campplus-sv-zh-en-coreml"},
	} {
		if c[0] != c[1] {
			t.Errorf("runs %s, want %s", c[0], c[1])
		}
	}
	if cfg.ASR.Provider != config.ProviderCoreML || !cfg.VAD.Enabled ||
		!cfg.API.DialectEnabled(config.DialectRealtime) {
		t.Error("with every build present, the provider, the VAD or realtime is not on")
	}
	for name, f := range features(s, p.Started, func(id string) bool { return slices.Contains(builds, id) }) {
		if f.State != StateReady {
			t.Errorf("%s: %s, missing %v", name, f.State, f.Missing)
		}
	}

	s.DefaultModel = "zipformer-small-en"
	if got := planWith(t, s).Config.ASR.DefaultModel; got != "zipformer-small-en" {
		t.Errorf("a model without a build runs as %s", got)
	}
	s.Provider = ""
	if got := planWith(t, s).Config.VAD.Model; got != config.Default().VAD.Model {
		t.Errorf("on the CPU the VAD is %s", got)
	}
}

func TestFeatureStates(t *testing.T) {
	diar := config.Default().Diarization.Model
	s := Settings{DefaultModel: "offline", RealtimeModel: "streaming", Diarization: true}
	started := planWith(t, s).Started // nothing on disk at the start

	state := func(s Settings, present ...string) map[string]string {
		out := map[string]string{}
		for name, f := range features(s, started, func(id string) bool { return slices.Contains(present, id) }) {
			out[name] = f.State
		}
		return out
	}

	got := state(s)
	for name, want := range map[string]string{
		FeatureVAD: StateMissing, FeatureTranscription: StateMissing, FeatureRealtime: StateMissing,
		FeatureDiarization: StateMissing, FeatureIsolation: StateOff,
	} {
		if got[name] != want {
			t.Errorf("nothing on disk: %s = %s, want %s", name, got[name], want)
		}
	}

	// Downloaded since: the lazy features serve now, the eager ones next time.
	got = state(s, "silero-vad-v5", "offline", "streaming", diar)
	for name, want := range map[string]string{
		FeatureVAD: StateRestart, FeatureTranscription: StateReady, FeatureRealtime: StateRestart,
		FeatureDiarization: StateReady,
	} {
		if got[name] != want {
			t.Errorf("downloaded: %s = %s, want %s", name, got[name], want)
		}
	}

	// A changed setting waits for the next start, and so does one turned off.
	changed := s
	changed.DefaultModel, changed.Diarization = "other", false
	got = state(changed, "silero-vad-v5", "offline", "other", "streaming", diar)
	if got[FeatureTranscription] != StateRestart || got[FeatureDiarization] != StateRestart {
		t.Errorf("changed settings: %v", got)
	}
}

func TestControllerSavesAndAnnouncesARestartOnce(t *testing.T) {
	paths := PathsFor(t.TempDir())
	if err := paths.Create(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	present := map[string]bool{}
	c := NewController(ControllerOptions{
		Paths:   paths,
		Started: planWith(t, Settings{}).Started,
		Present: func(id string) bool { return present[id] },
		Lookup:  catalog,
		Events:  NewEvents(&out),
	})
	if st := c.Status(); st.Configured || !slices.Equal(st.MissingModels, []string{"silero-vad-v5"}) {
		t.Fatalf("first start: %+v", st)
	}

	if _, err := c.Update([]byte(`{"default_model":"nope"}`)); err == nil {
		t.Fatal("an unknown model was accepted")
	}
	st, err := c.Update([]byte(`{"default_model":"offline"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Configured || st.RestartRequired || !slices.Equal(st.MissingModels, []string{"silero-vad-v5", "offline"}) {
		t.Fatalf("after setup: %+v", st)
	}
	if saved, configured, _ := LoadSettings(paths.Settings); !configured || saved.DefaultModel != "offline" {
		t.Fatalf("not saved: %+v", saved)
	}

	present["silero-vad-v5"] = true
	c.Changed()
	c.Changed()
	if n := strings.Count(out.String(), `"event":"restart_required"`); n != 1 {
		t.Fatalf("announced %d times:\n%s", n, out.String())
	}
	var ev struct {
		Event    string   `json:"event"`
		Features []string `json:"features"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &ev); err != nil || !slices.Equal(ev.Features, []string{"vad"}) {
		t.Fatalf("event %q: %v", out.String(), err)
	}
}

func TestEventsAreOneJSONObjectPerLine(t *testing.T) {
	var out bytes.Buffer
	e := NewEvents(&out)
	e.Emit(EventReady, map[string]any{"port": 1234, "token": "t"})
	e.Emit(EventStopping, nil)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], `{"event":"ready",`) || lines[1] != `{"event":"stopping"}` {
		t.Fatalf("lines: %q", lines)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &v); err != nil || v["port"] != float64(1234) {
		t.Fatalf("%q: %v", lines[0], err)
	}
}

func TestLockIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nanoasr.lock")
	first, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("a second lock = %v, want ErrLocked", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Lock(path)
	if err != nil {
		t.Fatalf("the lock was not given up: %v", err)
	}
	_ = again.Close()
}

func TestWatchParentNoticesTheExit(t *testing.T) {
	// The test binary itself, running only the helper below, is a process
	// that waits until it is killed on every platform.
	cmd := exec.Command(testBinary(t), "-test.run=^TestHelperWaits$", "-test.count=1")
	cmd.Env = append(os.Environ(), "NANOASR_TEST_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan string, 1)
	if err := WatchParent(ctx, cmd.Process.Pid, func(reason string) { stopped <- reason }); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	select {
	case reason := <-stopped:
		if reason != "parent exited" {
			t.Fatalf("reason %q", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the exit was not noticed")
	}
}

func TestWatchStdinStopsAtTheEnd(t *testing.T) {
	stopped := make(chan string, 1)
	WatchStdin(strings.NewReader("ignored"), func(reason string) { stopped <- reason })
	select {
	case reason := <-stopped:
		if reason != "stdin closed" {
			t.Fatalf("reason %q", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the end of stdin was not noticed")
	}
}
