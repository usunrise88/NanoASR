package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nanoasr.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalAuth = `
auth:
  mode: apikey
  keys:
    - sk-plain-0123456789abcdef
    - name: ci
      key: sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
      admin: true
`

func TestAPIKeyAcceptsBothForms(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalAuth))
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Auth.Keys) != 2 {
		t.Fatalf("got %d keys, want 2", len(cfg.Auth.Keys))
	}
	if got := cfg.Auth.Keys[0]; got.Key != "sk-plain-0123456789abcdef" || got.Admin {
		t.Errorf("bare string form parsed as %+v", got)
	}
	if got := cfg.Auth.Keys[1]; got.Name != "ci" || !got.Admin {
		t.Errorf("mapping form parsed as %+v", got)
	}
}

// yaml.v3 cannot read time.Duration on its own; the example configuration was
// unloadable until Duration got its own unmarshaller.
func TestDurationsParseFromTheFormOperatorsWrite(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalAuth+`
server:
  shutdown_grace: 45s
audio:
  max_duration: 2h
asr:
  idle_ttl: 0
`))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Server.ShutdownGrace.Duration != 45*time.Second {
		t.Errorf("shutdown_grace = %v", cfg.Server.ShutdownGrace)
	}
	if cfg.Audio.MaxDuration.Duration != 2*time.Hour {
		t.Errorf("max_duration = %v", cfg.Audio.MaxDuration)
	}
	// A bare number is a duration too: "0" is how an operator naturally writes
	// "off", and refusing it would be pedantry.
	if cfg.ASR.IdleTTL.Duration != 0 {
		t.Errorf("idle_ttl = %v, want 0 from a bare number", cfg.ASR.IdleTTL)
	}
}

func TestValidateRejectsUnusableAuth(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			// The shipped default before this milestone: looks configured,
			// answers 401 to everything.
			name: "apikey with no keys",
			body: "auth:\n  mode: apikey\n  keys: []\n",
			want: "no keys are configured",
		},
		{
			name: "key with no value",
			body: "auth:\n  mode: apikey\n  keys:\n    - name: broken\n      admin: true\n",
			want: "no key value",
		},
		{
			name: "open mode on a routable address",
			body: "auth:\n  mode: open\nserver:\n  addr: \"0.0.0.0:8080\"\n",
			want: "loopback",
		},
		{
			name: "unknown mode",
			body: "auth:\n  mode: mtls\n",
			want: "must be apikey or open",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, c.body))
			if err == nil {
				t.Fatalf("expected an error containing %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestOpenModeIsAllowedOnLoopback(t *testing.T) {
	if _, err := Load(writeConfig(t, "auth:\n  mode: open\nserver:\n  addr: \"127.0.0.1:8080\"\n")); err != nil {
		t.Fatalf("open mode on loopback should be allowed: %v", err)
	}
}

func TestEnvKeysReplaceFileKeysAndAreNeverAdmin(t *testing.T) {
	t.Setenv("NANOASR_AUTH_KEYS", "sk-from-env-0123456789, sk-second-0123456789 ")

	cfg, err := Load(writeConfig(t, minimalAuth))
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Auth.Keys) != 2 {
		t.Fatalf("got %d keys, want the two from the environment", len(cfg.Auth.Keys))
	}
	for _, k := range cfg.Auth.Keys {
		if k.Admin {
			// Administrative rights should take a deliberate edit to a file,
			// not an environment variable copied between deployments.
			t.Errorf("key %q from the environment claims admin rights", k.Key)
		}
		if strings.TrimSpace(k.Key) != k.Key {
			t.Errorf("key %q was not trimmed", k.Key)
		}
	}
}

func TestRedactRemovesPlaintextSecrets(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalAuth))
	if err != nil {
		t.Fatal(err)
	}
	before := cfg.Auth.Keys[1].Key

	cfg.Auth.Redact()

	if got := cfg.Auth.Keys[0].Key; !strings.HasPrefix(got, "sha256:") {
		t.Errorf("plaintext key survived redaction: %q", got)
	}
	if got := cfg.Auth.Keys[1].Key; got != before {
		t.Errorf("an already-hashed key was rewritten: %q", got)
	}
}

func TestAutotuneStaysWithinMemory(t *testing.T) {
	cfg := Default()
	cfg.Autotune()

	if cfg.ASR.InferenceSlots < 1 || cfg.ASR.NumThreads < 1 || cfg.Jobs.MaxConcurrent < 1 {
		t.Fatalf("autotune produced unusable sizing: %+v", cfg.ASR)
	}
	// A ten-minute file is ~115 MB of float32; the estimate must account for
	// every concurrent job, not just the models.
	if est := cfg.PeakMemoryEstimateMB(); est <= cfg.ASR.MaxModelRSSMB {
		t.Errorf("peak estimate %d MB ignores decoded audio", est)
	}
}

func TestUnknownConfigKeyIsAStartupFailure(t *testing.T) {
	// A typo in a field that matters — feature dim, thread count — should not
	// silently fall back to a default.
	_, err := Load(writeConfig(t, minimalAuth+"asr:\n  num_thread: 4\n"))
	if err == nil {
		t.Fatal("an unknown configuration key was accepted")
	}
}
