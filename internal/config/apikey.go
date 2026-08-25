package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// UnmarshalYAML accepts either form an API key can take in configuration.
//
// The bare-string form is what most deployments need and what
// NANOASR_AUTH_KEYS produces; the mapping form exists for keys that need a
// name in the logs or administrative rights.
func (k *APIKey) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var secret string
		if err := node.Decode(&secret); err != nil {
			return fmt.Errorf("line %d: an api key must be a string or a mapping", node.Line)
		}
		k.Key = strings.TrimSpace(secret)
		return nil
	}

	// The alias avoids recursing back into this method.
	type plain APIKey
	var p plain
	if err := node.Decode(&p); err != nil {
		return err
	}
	*k = APIKey(p)
	k.Key = strings.TrimSpace(k.Key)
	return nil
}

// Redact replaces plaintext secrets with their hashed form.
//
// Called once the key store is built, so the in-memory configuration — which
// /api/v1/config will eventually serve — no longer holds anything usable.
func (a *Auth) Redact() {
	for i := range a.Keys {
		secret := a.Keys[i].Key
		if secret == "" || strings.HasPrefix(secret, "sha256:") {
			continue
		}
		a.Keys[i].Key = HashSecret(secret)
	}
}

// HashSecret is the stored form of a credential: the digest the server
// compares against, so that a configuration file need not hold anything usable.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "sha256:" + hex.EncodeToString(sum[:])
}
