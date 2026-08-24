package httpx

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
)

// API keys are verified with SHA-256 and a constant-time comparison, not with
// a password KDF.
//
// SPEC §11 originally said argon2id. That is the right tool for low-entropy
// passwords and the wrong one here: an API key is 256 bits of randomness that
// nobody brute-forces, while argon2 costs tens of milliseconds of CPU per
// request. On a server whose CPU is deliberately rationed for inference, that
// turns a flood of junk keys into a way to take cores away from recognition.
// SHA-256 over a high-entropy secret gives the same protection at no cost.

// KeySpec is one configured credential, before hashing.
type KeySpec struct {
	Name string
	// Secret is either the key itself or "sha256:<hex>" when the operator
	// prefers not to keep the plaintext in a config file.
	Secret string
	Admin  bool
	// RPS is this key's request rate limit, 0 meaning unlimited.
	RPS float64
}

// Key is a verified credential. The secret is not retained.
type Key struct {
	ID    string
	Name  string
	Admin bool
	RPS   float64

	digest [sha256.Size]byte
}

// StaticKeyStore holds the keys from configuration. It is immutable after
// construction, so it needs no locking; rotating a key is a restart.
type StaticKeyStore struct {
	keys []Key
}

// NewStaticKeyStore hashes every configured key.
func NewStaticKeyStore(specs []KeySpec) (*StaticKeyStore, error) {
	s := &StaticKeyStore{}
	seen := map[string]string{}

	for i, spec := range specs {
		secret := strings.TrimSpace(spec.Secret)
		if secret == "" {
			return nil, fmt.Errorf("auth key %d (%q) has no value", i, spec.Name)
		}

		digest, err := parseSecret(secret)
		if err != nil {
			return nil, fmt.Errorf("auth key %d (%q): %w", i, spec.Name, err)
		}

		id := hex.EncodeToString(digest[:4])
		if prev, dup := seen[id]; dup {
			// Two identical keys under different names is a configuration
			// mistake that would otherwise silently pick one of them.
			return nil, fmt.Errorf("auth key %q duplicates %q", spec.Name, prev)
		}
		seen[id] = spec.Name

		name := spec.Name
		if name == "" {
			name = "key-" + id
		}
		s.keys = append(s.keys, Key{
			ID: id, Name: name, Admin: spec.Admin, RPS: spec.RPS, digest: digest,
		})
	}
	return s, nil
}

// parseSecret accepts a plaintext key or a pre-hashed "sha256:<hex>" value.
func parseSecret(secret string) ([sha256.Size]byte, error) {
	var out [sha256.Size]byte

	if rest, ok := strings.CutPrefix(secret, "sha256:"); ok {
		raw, err := hex.DecodeString(strings.TrimSpace(rest))
		if err != nil {
			return out, fmt.Errorf("sha256: value is not hexadecimal: %w", err)
		}
		if len(raw) != sha256.Size {
			return out, fmt.Errorf("sha256: digest is %d bytes, want %d", len(raw), sha256.Size)
		}
		copy(out[:], raw)
		return out, nil
	}

	if len(secret) < 16 {
		// Short keys are guessable regardless of how they are stored.
		return out, fmt.Errorf("key must be at least 16 characters, got %d", len(secret))
	}
	return sha256.Sum256([]byte(secret)), nil
}

// Verify checks a bearer token.
//
// Every key is compared, without an early exit on the first match: returning
// as soon as a key matches would make the response time depend on a key's
// position in the list.
func (s *StaticKeyStore) Verify(_ context.Context, token string) (string, bool) {
	got := sha256.Sum256([]byte(token))

	match := -1
	for i := range s.keys {
		if subtle.ConstantTimeCompare(got[:], s.keys[i].digest[:]) == 1 {
			match = i
		}
	}
	if match < 0 {
		return "", false
	}
	return s.keys[match].ID, true
}

// Lookup returns a key by its id, for authorisation decisions after
// authentication has already succeeded.
func (s *StaticKeyStore) Lookup(id string) (Key, bool) {
	for _, k := range s.keys {
		if k.ID == id {
			return k, true
		}
	}
	return Key{}, false
}

func (s *StaticKeyStore) Len() int { return len(s.keys) }

// Names lists key names and ids for startup logging. Never the secrets.
func (s *StaticKeyStore) Names() []string {
	out := make([]string, 0, len(s.keys))
	for _, k := range s.keys {
		suffix := ""
		if k.Admin {
			suffix = " (admin)"
		}
		out = append(out, k.Name+"/"+k.ID+suffix)
	}
	return out
}

// Hash renders a secret in the form the configuration accepts, so an operator
// can move a plaintext key out of a config file without inventing the format.
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "sha256:" + hex.EncodeToString(sum[:])
}

var _ KeyStore = (*StaticKeyStore)(nil)
