package sortformer

import (
	"bufio"
	"os"
	"slices"
	"strings"
)

// loadedRuntime lists every onnxruntime file mapped into the process.
func loadedRuntime() ([]string, error) {
	f, err := os.Open("/proc/self/maps")
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var paths []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 {
			continue
		}
		if p := fields[len(fields)-1]; strings.Contains(p, "libonnxruntime") && !slices.Contains(paths, p) {
			paths = append(paths, p)
		}
	}
	return paths, sc.Err()
}
