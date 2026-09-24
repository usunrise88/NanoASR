package sortformer

import (
	"bufio"
	"os"
	"runtime"
	"strings"
	"testing"

	sonnx "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
	ort "github.com/yalue/onnxruntime_go"
)

func TestEnvironmentComesUp(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skipf("not supported on %s", runtime.GOOS)
	}
	if err := ensureEnvironment(); err != nil {
		t.Fatalf("ensureEnvironment: %v", err)
	}
	// Twice is the same as once: the second caller must not re-initialise.
	if err := ensureEnvironment(); err != nil {
		t.Fatalf("second ensureEnvironment: %v", err)
	}
	if !ort.IsInitialized() {
		t.Fatal("onnxruntime reports itself uninitialised")
	}
	t.Logf("onnxruntime %s, sherpa-onnx sees %s", ort.GetVersion(), sonnx.GetOnnxruntimeVersion())
}

// The whole point of loading by soname: sherpa-onnx and this package must share
// one onnxruntime. Two mapped copies would each keep their own thread pool and
// allocator while believing they were alone.
func TestOneOnnxruntimeInTheProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc/self/maps is a Linux check; Windows is checked by module handle in the release job")
	}
	if err := ensureEnvironment(); err != nil {
		t.Fatalf("ensureEnvironment: %v", err)
	}
	f, err := os.Open("/proc/self/maps")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	paths := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 {
			continue
		}
		if p := fields[len(fields)-1]; strings.Contains(p, "libonnxruntime") {
			paths[p] = true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected one onnxruntime mapped, found %d: %v", len(paths), paths)
	}
}

func TestSameVersion(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"1.27.1", "1.27.1", true},
		{"v1.27.1", "1.27.1", true},
		{"1.27.1 ", "1.27.1+abc", true},
		{"1.27.1", "1.27.0", false},
		{"", "", false},
	} {
		if got := sameVersion(tc.a, tc.b); got != tc.want {
			t.Errorf("sameVersion(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
