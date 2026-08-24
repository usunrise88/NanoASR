package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Snapshot renders the effective configuration for GET /api/v1/config.
//
// It goes through YAML rather than straight to JSON so the field names match
// the configuration file exactly — an operator can diff what the server says it
// is running against what they wrote. The alternative, a second set of json
// tags on every struct, is a second set of names that can drift from the first
// with nothing to catch it.
//
// Secrets: Auth.Redact must already have run, which it does at startup once the
// key store holds the digests. This makes sure of it anyway, because a config
// endpoint that leaks a key is worse than one that does not exist.
func (c Config) Snapshot() (map[string]any, error) {
	c.Auth.Redact()

	encoded, err := yaml.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("config: render snapshot: %w", err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(encoded, &out); err != nil {
		return nil, fmt.Errorf("config: render snapshot: %w", err)
	}
	return out, nil
}
