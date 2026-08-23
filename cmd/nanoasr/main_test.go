package main

import (
	"flag"
	"testing"
)

// The standard flag package stops at the first positional argument, which
// silently drops every flag after it — the command runs with defaults and
// nothing warns. This bit three subcommands before the arguments were
// partitioned before parsing.
func TestParseFlagsAcceptsAnyOrder(t *testing.T) {
	cases := []struct {
		name           string
		args           []string
		wantConfig     string
		wantVerbose    bool
		wantPositional []string
	}{
		{
			name:           "flag first",
			args:           []string{"-config", "a.yaml", "model-one", "model-two"},
			wantConfig:     "a.yaml",
			wantPositional: []string{"model-one", "model-two"},
		},
		{
			name:           "flag last",
			args:           []string{"model-one", "model-two", "-config", "a.yaml"},
			wantConfig:     "a.yaml",
			wantPositional: []string{"model-one", "model-two"},
		},
		{
			name:           "flag between",
			args:           []string{"model-one", "-config", "a.yaml", "model-two"},
			wantConfig:     "a.yaml",
			wantPositional: []string{"model-one", "model-two"},
		},
		{
			name:           "equals form",
			args:           []string{"model-one", "--config=a.yaml"},
			wantConfig:     "a.yaml",
			wantPositional: []string{"model-one"},
		},
		{
			// A boolean flag must not swallow the argument after it.
			name:           "boolean flag before a positional",
			args:           []string{"-verbose", "model-one"},
			wantVerbose:    true,
			wantPositional: []string{"model-one"},
		},
		{
			name:           "double dash ends flags",
			args:           []string{"--", "-not-a-flag"},
			wantPositional: []string{"-not-a-flag"},
		},
		{
			name:           "no arguments",
			args:           nil,
			wantPositional: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(nopWriter{})
			config := fs.String("config", "", "")
			verbose := fs.Bool("verbose", false, "")

			got, err := parseFlags(fs, c.args)
			if err != nil {
				t.Fatal(err)
			}
			if *config != c.wantConfig {
				t.Errorf("config = %q, want %q", *config, c.wantConfig)
			}
			if *verbose != c.wantVerbose {
				t.Errorf("verbose = %v, want %v", *verbose, c.wantVerbose)
			}
			if len(got) != len(c.wantPositional) {
				t.Fatalf("positional = %v, want %v", got, c.wantPositional)
			}
			for i := range got {
				if got[i] != c.wantPositional[i] {
					t.Errorf("positional = %v, want %v", got, c.wantPositional)
					break
				}
			}
		})
	}
}

func TestTakePositional(t *testing.T) {
	if got, rest := takePositional([]string{"pull", "-config", "x"}, "list"); got != "pull" || len(rest) != 2 {
		t.Errorf("takePositional = %q, %v", got, rest)
	}
	if got, rest := takePositional([]string{"-config", "x"}, "list"); got != "list" || len(rest) != 2 {
		t.Errorf("a leading flag should leave the fallback: %q, %v", got, rest)
	}
	if got, _ := takePositional(nil, "list"); got != "list" {
		t.Errorf("empty args = %q, want the fallback", got)
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{
		0: "-", -1: "-", 512: "0K", 2048: "2K", 5 << 20: "5M", 3 << 30: "3.0G",
	} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
