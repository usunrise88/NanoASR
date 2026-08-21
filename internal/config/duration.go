package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that YAML can actually read.
//
// gopkg.in/yaml.v3 has no special handling for time.Duration: it sees an int64
// and refuses "10s" outright, which meant the shipped example configuration
// could not be loaded at all. Wrapping it here keeps the config file written
// the way an operator expects.
type Duration struct {
	time.Duration
}

// Dur builds a Duration from a time.Duration.
func Dur(d time.Duration) Duration { return Duration{d} }

// UnmarshalYAML accepts "1500ms", "30s", "2h" and a bare number of seconds.
// The bare number exists because "keep_audio_ttl: 0" is the natural way to
// write "off", and failing on it would be pedantry.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err == nil {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("line %d: %q is not a duration such as 30s or 5m", node.Line, s)
		}
		d.Duration = parsed
		return nil
	}

	var seconds float64
	if err := node.Decode(&seconds); err != nil {
		return fmt.Errorf("line %d: expected a duration such as 30s, got %q", node.Line, node.Value)
	}
	d.Duration = time.Duration(seconds * float64(time.Second))
	return nil
}

// MarshalYAML writes durations back in the form they were written in.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }
