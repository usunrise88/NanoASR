package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/usunrise88/nanoasr/internal/config"
)

// renderTestInit is what initCommand does up to the point where it would start
// downloading gigabytes.
func renderTestInit(t *testing.T, diarize bool) []byte {
	t.Helper()
	adminKey, err := config.NewKeySecret()
	if err != nil {
		t.Fatalf("NewKeySecret: %v", err)
	}
	userKey, err := config.NewKeySecret()
	if err != nil {
		t.Fatalf("NewKeySecret: %v", err)
	}
	b, err := renderInit(map[string]string{
		"Host":      "test",
		"Addr":      "127.0.0.1:8080",
		"AdminKey":  adminKey,
		"UserKey":   userKey,
		"ModelsDir": "/var/lib/nanoasr/models",
		"DBPath":    "/var/lib/nanoasr/nanoasr.db",
		"ASRModel":  initASRModel,
		"VADModel":  initVADModel,
		"SegModel":  quoteIfEmpty(pick(diarize, initSegModel)),
		"EmbModel":  quoteIfEmpty(pick(diarize, initEmbModel)),
		"Diarize":   map[bool]string{true: "true", false: "false"}[diarize],
		"Threshold": defaultThreshold(),
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
		if err := os.WriteFile(path, renderTestInit(t, diarize), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("diarize=%v: the generated configuration did not load: %v", diarize, err)
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
	if err := os.WriteFile(path, renderTestInit(t, true), 0o600); err != nil {
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
	for _, id := range initModels(initASRModel, true) {
		if !strings.Contains(catalog, "- id: "+id+"\n") {
			t.Errorf("init downloads %q, which is not in internal/registry/catalog.yaml", id)
		}
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
