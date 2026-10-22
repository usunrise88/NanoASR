package registry

import (
	"strings"
	"testing"
)

// A CoreML build is found by its name alone (asr.provider: coreml runs a
// model's build in its place), so every one must be named after a model in
// the catalog that does the same job.
func TestCoreMLBuildsAreNamedAfterTheirModel(t *testing.T) {
	entries, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Manifest{}
	for _, m := range entries {
		byID[m.ID] = m
	}
	builds := 0
	for _, m := range entries {
		base, ok := strings.CutSuffix(m.ID, CoreMLSuffix)
		if !ok {
			if b := CoreMLBuild(m.ID); b != "" && b != m.ID+CoreMLSuffix {
				t.Errorf("%s: CoreMLBuild gives %s", m.ID, b)
			}
			continue
		}
		builds++
		orig, ok := byID[base]
		if !ok {
			t.Errorf("%s: the catalog has no %s for it to be the build of", m.ID, base)
			continue
		}
		if orig.EffectiveKind() != m.EffectiveKind() || orig.Streaming != m.Streaming {
			t.Errorf("%s is a %s (streaming %v), %s a %s (streaming %v)", m.ID, m.EffectiveKind(), m.Streaming,
				base, orig.EffectiveKind(), orig.Streaming)
		}
		if got := CoreMLBuild(base); got != m.ID {
			t.Errorf("CoreMLBuild(%s) = %q, want %s", base, got, m.ID)
		}
		if got := CoreMLBuild(m.ID); got != m.ID {
			t.Errorf("CoreMLBuild(%s) = %q, want itself", m.ID, got)
		}
	}
	if builds == 0 {
		t.Fatal("the catalog has no CoreML builds")
	}
	if got := CoreMLBuild("zipformer-small-en"); got != "" {
		t.Errorf("a model without a build maps to %q", got)
	}
}
