package config

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const sample = `# NanoASR configuration.
#
# Written by hand and worth keeping that way.
server:
  addr: "127.0.0.1:8080"

auth:
  # Two keys, and a comment explaining why.
  mode: apikey
  keys:
    - name: admin
      key: sk-nanoasr-aaaa
      admin: true
    - name: user
      key: sk-nanoasr-bbbb

log:
  level: info
`

func parse(t *testing.T, s string) *Document {
	t.Helper()
	doc, err := ParseDocument([]byte(s))
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	return doc
}

// The whole reason Document exists: `nanoasr key issue` must not cost the
// operator the comments they wrote.
func TestDocumentKeepsCommentsAndSpacing(t *testing.T) {
	doc := parse(t, sample)
	if err := doc.AddKey(APIKey{Name: "ci", Key: "sk-nanoasr-cccc", RPS: 5}); err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	b, err := doc.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	out := string(b)

	for _, want := range []string{
		"# NanoASR configuration.",
		"# Written by hand and worth keeping that way.",
		"# Two keys, and a comment explaining why.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("comment %q did not survive the edit:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "\n\nauth:") {
		t.Errorf("top-level blocks lost their blank line:\n%s", out)
	}
}

func TestAddAndRemoveKey(t *testing.T) {
	doc := parse(t, sample)
	if err := doc.AddKey(APIKey{Name: "ci", Key: "sk-nanoasr-cccc", RPS: 2.5,
		Priority: PriorityInteractive}); err != nil {
		t.Fatalf("AddKey: %v", err)
	}

	keys := doc.Keys()
	if len(keys) != 3 {
		t.Fatalf("got %d keys, want 3", len(keys))
	}
	got := keys[2]
	if got.Name != "ci" || got.RPS != 2.5 || !got.Interactive() || got.Admin {
		t.Errorf("round-tripped key = %+v", got)
	}

	if !doc.RemoveKey("ci") {
		t.Fatal("RemoveKey(ci) = false")
	}
	if len(doc.Keys()) != 2 {
		t.Errorf("got %d keys after removal, want 2", len(doc.Keys()))
	}
	if doc.RemoveKey("ci") {
		t.Error("removing a key twice reported success the second time")
	}
}

// A key issued before names were used can still be revoked by its secret.
func TestRemoveKeyBySecret(t *testing.T) {
	doc := parse(t, sample)
	if !doc.RemoveKey("sk-nanoasr-bbbb") {
		t.Fatal("RemoveKey by secret = false")
	}
	if n := len(doc.Keys()); n != 1 {
		t.Errorf("got %d keys, want 1", n)
	}
}

// Names address keys everywhere else, so two of them would make `key remove`
// ambiguous in a file the operator cannot see.
func TestAddKeyRefusesADuplicate(t *testing.T) {
	doc := parse(t, sample)
	if err := doc.AddKey(APIKey{Name: "admin", Key: "sk-nanoasr-dddd"}); err == nil {
		t.Error("adding a second key named admin was allowed")
	}
	if err := doc.AddKey(APIKey{Name: "other", Key: "sk-nanoasr-aaaa"}); err == nil {
		t.Error("adding a duplicate secret was allowed")
	}
}

// An empty file is what an operator has after `touch nanoasr.yaml`, and the
// first `key issue` should work rather than complain.
func TestAddKeyToAnEmptyDocument(t *testing.T) {
	doc := parse(t, "")
	if err := doc.AddKey(APIKey{Name: "admin", Key: "sk-nanoasr-eeee", Admin: true}); err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	b, err := doc.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}

	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("the generated document did not load: %v\n%s", err, b)
	}
	if len(cfg.Auth.Keys) != 1 || !cfg.Auth.Keys[0].Admin {
		t.Errorf("loaded keys = %+v", cfg.Auth.Keys)
	}
}

func TestParseDocumentRejectsANonMapping(t *testing.T) {
	if _, err := ParseDocument([]byte("- a\n- b\n")); err == nil {
		t.Error("a top-level sequence was accepted as a configuration file")
	}
}

func TestNewKeySecret(t *testing.T) {
	seen := map[string]bool{}
	for range 64 {
		s, err := NewKeySecret()
		if err != nil {
			t.Fatalf("NewKeySecret: %v", err)
		}
		if !strings.HasPrefix(s, KeyPrefix) {
			t.Fatalf("secret %q does not start with %q", s, KeyPrefix)
		}
		body := strings.TrimPrefix(s, KeyPrefix)
		if len(body) != 32 {
			t.Fatalf("secret body is %d characters, want 32", len(body))
		}
		if strings.ContainsAny(body, "0O1lI") {
			t.Errorf("secret %q contains a character that is misread when typed back in", s)
		}
		if seen[s] {
			t.Fatalf("NewKeySecret repeated itself: %q", s)
		}
		seen[s] = true
	}
}

func TestHashSecretIsWhatRedactWrites(t *testing.T) {
	a := Auth{Keys: []APIKey{{Key: "sk-nanoasr-aaaa"}}}
	a.Redact()
	if want := HashSecret("sk-nanoasr-aaaa"); a.Keys[0].Key != want {
		t.Errorf("Redact wrote %q, HashSecret says %q", a.Keys[0].Key, want)
	}
}
