package sortformer

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// goldenDir holds what scripts/diar-research/golden_sortformer.py produced from
// the transformers reference at a pinned revision.
var goldenDir = filepath.Join("..", "..", "..", "testdata", "golden", "sortformer")

// splitmix64 and unit are bit-identical to the generator's, so a test can
// regenerate every fake logit the reference saw instead of reading it from disk.
func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	z := x
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func unit(key uint64) float64 {
	return float64(splitmix64(key)>>11) * (1.0 / (1 << 53))
}

// scenario mirrors a cache.json scenario.
type scenario struct {
	Name     string `json:"name"`
	Seed     int64  `json:"seed"`
	Speakers int    `json:"speakers"`
	Run      int    `json:"run"`
	Frames   int    `json:"frames"`
	Overlap  bool   `json:"overlap"`
	Steps    []struct {
		Input      []int `json:"input"`
		Cache      []int `json:"cache"`
		FIFO       []int `json:"fifo"`
		Compressed bool  `json:"compressed"`
	} `json:"steps"`
}

// fakeLogit is golden_sortformer.py's fake_logit: the logit of one 10 ms frame
// for one speaker, as a function of the recording frame it came from.
func fakeLogit(sc scenario, frame, sub, speaker int) float32 {
	if frame < 0 {
		return -6
	}
	turn := (frame / sc.Run) % sc.Speakers
	active := speaker == turn
	if sc.Overlap && frame%sc.Run < sc.Run/3 {
		active = active || speaker == (turn+1)%sc.Speakers
	}
	key := ((uint64(sc.Seed)*1_000_003+uint64(frame))*8+uint64(sub))*8 + uint64(speaker)
	noise := (unit(key) - 0.5) * 4.0
	base := -4.0
	if active {
		base = 4.0
	}
	return float32(base + noise)
}

func readJSON(t testing.TB, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenDir, name))
	if err != nil {
		t.Fatalf("golden data missing: %v", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func readFloat32s(t testing.TB, name string) []float32 {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenDir, name))
	if err != nil {
		t.Fatalf("golden data missing: %v", err)
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out
}
