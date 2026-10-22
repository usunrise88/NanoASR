//go:build darwin

package coreml

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPredict loads converted models from NANOASR_COREML_TEST_DIR and runs
// each once per input: the bridge's conversions, strides and errors against
// models with fp16 inputs and outputs. Skipped without the directory, which
// only a machine that has run the conversion scripts has.
func TestPredict(t *testing.T) {
	dir := os.Getenv("NANOASR_COREML_TEST_DIR")
	if dir == "" {
		t.Skip("NANOASR_COREML_TEST_DIR is not set")
	}
	cases := []struct{ file, function string }{
		{"silero_n128.mlmodelc", ""},
		{"rnnt_step_b8_w16.mlmodelc", ""},
		{"gigaam_enc.mlmodelc", "t400"},
	}
	for _, c := range cases {
		path := filepath.Join(dir, c.file)
		if _, err := os.Stat(path); err != nil {
			t.Logf("skipping %s: %v", c.file, err)
			continue
		}
		t.Run(c.file+c.function, func(t *testing.T) {
			t0 := time.Now()
			m, err := Load(path, c.function, NeuralEngine)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			load := time.Since(t0)
			d := m.Description()
			in, out := map[string][]float32{}, map[string][]float32{}
			for _, f := range d.Inputs {
				buf := make([]float32, f.Size())
				for i := range buf {
					buf[i] = 1
				}
				in[f.Name] = buf
			}
			for _, f := range d.Outputs {
				out[f.Name] = make([]float32, f.Size())
			}
			if err := m.Predict(in, out); err != nil {
				t.Fatal(err)
			}
			const runs = 20
			t0 = time.Now()
			for range runs {
				if err := m.Predict(in, out); err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("%s: load %v, %v per prediction; inputs %v outputs %v",
				c.file, load.Round(time.Millisecond), time.Since(t0)/runs, d.Inputs, d.Outputs)

			// The wrong size is refused, not read past.
			for name, buf := range in {
				in[name] = buf[:len(buf)-1]
				if err := m.Predict(in, out); err == nil {
					t.Errorf("a short %s was accepted", name)
				}
				in[name] = buf
				break
			}
		})
	}
}
