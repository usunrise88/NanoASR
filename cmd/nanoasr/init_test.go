package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/usunrise88/nanoasr/internal/config"
)

// renderTestInit is what initCommand does up to the point where it would start
// downloading gigabytes.
func renderTestInit(t *testing.T, diarize, realtime bool) []byte {
	t.Helper()
	adminKey, err := config.NewKeySecret()
	if err != nil {
		t.Fatalf("NewKeySecret: %v", err)
	}
	userKey, err := config.NewKeySecret()
	if err != nil {
		t.Fatalf("NewKeySecret: %v", err)
	}
	dialects := []string{config.DialectOpenAI, config.DialectNative, config.DialectEra}
	if realtime {
		dialects = append(dialects, config.DialectRealtime)
	}
	b, err := renderInit(map[string]string{
		"Host":          "test",
		"Addr":          "127.0.0.1:8080",
		"Dialects":      strings.Join(dialects, ", "),
		"Provider":      config.ProviderCPU,
		"RealtimeModel": pick(realtime, initRealtimeModel),
		"AdminKey":      adminKey,
		"UserKey":       userKey,
		"ModelsDir":     "/var/lib/nanoasr/models",
		"DBPath":        "/var/lib/nanoasr/nanoasr.db",
		"ASRModel":      initASRModel,
		"VADModel":      initVADModel,
		"SegModel":      quoteIfEmpty(pick(diarize, initSegModel)),
		"EmbModel":      quoteIfEmpty(pick(diarize, initEmbModel)),
		"Diarize":       map[bool]string{true: "true", false: "false"}[diarize],
		"DiarModel":     initDiarModel,
		"Threshold":     defaultThreshold(),
	})
	if err != nil {
		t.Fatalf("renderInit: %v", err)
	}
	return b
}

// The template and the config schema live in different files and drift apart
// silently: a renamed key would only show up when an operator ran init and the
// server then refused to start. KnownFields makes that a test failure instead.
func TestInitTemplateLoads(t *testing.T) {
	for _, diarize := range []bool{true, false} {
		path := filepath.Join(t.TempDir(), "nanoasr.yaml")
		if err := os.WriteFile(path, renderTestInit(t, diarize, false), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("diarize=%v: the generated configuration did not load: %v", diarize, err)
		}
		if cfg.ASR.Provider != config.ProviderCPU {
			t.Errorf("asr.provider = %q, want %q", cfg.ASR.Provider, config.ProviderCPU)
		}
		if cfg.API.DialectEnabled(config.DialectRealtime) {
			t.Error("the realtime dialect is on without -realtime")
		}
		if cfg.ASR.DefaultModel != initASRModel {
			t.Errorf("default_model = %q, want %q", cfg.ASR.DefaultModel, initASRModel)
		}
		if cfg.Diarization.Enabled != diarize {
			t.Errorf("diarization.enabled = %v, want %v", cfg.Diarization.Enabled, diarize)
		}
		if len(cfg.Auth.Keys) != 2 {
			t.Fatalf("got %d keys, want an admin and a user", len(cfg.Auth.Keys))
		}
		if !cfg.Auth.Keys[0].Admin || cfg.Auth.Keys[1].Admin {
			t.Errorf("keys = %+v, want exactly the first to be administrative", cfg.Auth.Keys)
		}
	}
}

// The two secrets must differ. Rendering them from one call would be an easy
// mistake to make and an unpleasant one to discover.
func TestInitIssuesTwoDistinctKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nanoasr.yaml")
	if err := os.WriteFile(path, renderTestInit(t, true, false), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Keys[0].Key == cfg.Auth.Keys[1].Key {
		t.Error("the admin and user keys are the same secret")
	}
}

// The models init downloads have to exist in the catalog, or the first thing a
// new installation does is fail.
func TestInitModelsAreInTheCatalog(t *testing.T) {
	catalog := string(catalogSource(t))
	for _, id := range initModels(initASRModel, true, true) {
		if !strings.Contains(catalog, "- id: "+id+"\n") {
			t.Errorf("init downloads %q, which is not in internal/registry/catalog.yaml", id)
		}
	}
}

// `init -realtime` writes a second block and names a streaming model. The
// block is only read when the dialect is on, so a template that got either
// half wrong would produce a server that starts and serves no websocket.
func TestInitWithRealtimeLoadsAndNamesAStreamingModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nanoasr.yaml")
	if err := os.WriteFile(path, renderTestInit(t, false, true), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the generated configuration did not load: %v", err)
	}
	if !cfg.API.DialectEnabled(config.DialectRealtime) {
		t.Error("the realtime dialect is not enabled")
	}
	if cfg.Realtime.Model != initRealtimeModel {
		t.Errorf("realtime.model = %q, want %q", cfg.Realtime.Model, initRealtimeModel)
	}
	if !cfg.Realtime.Endpoint.Enabled || cfg.Realtime.Endpoint.Rule2MinTrailingSilence == 0 {
		t.Errorf("endpointing is not configured: %+v", cfg.Realtime.Endpoint)
	}
	// The model it names has to be a streaming one, or the server will refuse
	// to load it at startup.
	catalog := string(catalogSource(t))
	entry := strings.Index(catalog, "- id: "+initRealtimeModel+"\n")
	if entry < 0 {
		t.Fatalf("%s is not in the catalog", initRealtimeModel)
	}
	if !strings.Contains(catalog[entry:entry+400], "streaming: true") {
		t.Errorf("%s is not marked streaming in the catalog", initRealtimeModel)
	}
}

func catalogSource(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "registry", "catalog.yaml"))
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	return b
}

// `models pull -configured` has to name every model the server will load, or
// an upgrade downloads something at the first request instead of up front.
func TestConfiguredModelsCoverTheRealtimeDialect(t *testing.T) {
	cfg := config.Default()
	cfg.ASR.DefaultModel = initASRModel
	cfg.VAD.Enabled = true
	cfg.Diarization.Enabled = false

	// Off: realtime.model has a default in every configuration, so pulling it
	// unasked would be 128 MB of surprise.
	cfg.API.Dialects = []string{config.DialectOpenAI}
	if got := configuredModels(cfg); slices.Contains(got, cfg.Realtime.Model) {
		t.Errorf("the streaming model is fetched without the dialect: %v", got)
	}

	// On: it is the one model the dialect cannot start without.
	cfg.API.Dialects = []string{config.DialectOpenAI, config.DialectRealtime}
	got := configuredModels(cfg)
	if !slices.Contains(got, cfg.Realtime.Model) {
		t.Errorf("configuredModels(%v) = %v, missing the streaming model %q",
			cfg.API.Dialects, got, cfg.Realtime.Model)
	}
	if !slices.Contains(got, initASRModel) || !slices.Contains(got, cfg.VAD.Model) {
		t.Errorf("configuredModels dropped something else: %v", got)
	}
}
