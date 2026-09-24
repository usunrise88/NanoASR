// Package sortformer binds internal/diarize to NVIDIA's Nemotron-3-Diarization,
// an end-to-end Sortformer that orders speakers by first arrival and so has no
// clustering step and no threshold to tune.
//
// The model runs as two ONNX graphs through onnxruntime directly, not through
// sherpa-onnx, which has no Sortformer support. What sherpa-onnx does provide is
// the onnxruntime library itself: this package loads that same copy rather than
// shipping a second one, so the process keeps one onnxruntime however many
// engines use it.
package sortformer

import (
	"fmt"
	"runtime"
	"strings"
	"sync"

	sonnx "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
	ort "github.com/yalue/onnxruntime_go"

	"github.com/usunrise88/nanoasr/internal/core"
)

// runtimeLibrary is onnxruntime under the name the dynamic loader already knows
// it by.
//
// Not a path, deliberately. sherpa-onnx links the library by its soname, so by
// the time this runs the loader has it mapped, and asking for the bare name
// returns that mapping instead of searching the disk. A path would work in a
// release archive and fail under go test, where the library lives in the module
// cache; the bare name works in both, and cannot pick up a different build.
//
// On Windows the same holds for onnxruntime.dll, with one hazard: Windows 11
// ships its own, older onnxruntime.dll in System32. The bare name is safe only
// because sherpa's import table has already loaded ours by then, and a module
// already loaded is found before any search begins.
func runtimeLibrary() (string, error) {
	switch runtime.GOOS {
	case "linux":
		return "libonnxruntime.so", nil
	case "windows":
		return "onnxruntime.dll", nil
	default:
		// macOS links the dylib by an install name a bare dlopen does not
		// match, and the release matrix builds no darwin binary anyway.
		return "", fmt.Errorf("the sortformer diarizer is not supported on %s", runtime.GOOS)
	}
}

var (
	envOnce sync.Once
	envErr  error
)

// ensureEnvironment brings up the onnxruntime environment once per process.
//
// It is never torn down. Diarizers come and go — every test builds several — but
// the environment belongs to the process, and destroying it under a live
// session is undefined behaviour for no gain: the library stays mapped either
// way, because sherpa-onnx holds it.
//
// A failure is remembered rather than retried: every way this can fail is a
// property of the build or the host, and none of them fixes itself.
func ensureEnvironment() error {
	envOnce.Do(func() {
		name, err := runtimeLibrary()
		if err != nil {
			envErr = core.Errorf(core.CodeCapabilityUnavailable, "%s", err.Error())
			return
		}
		ort.SetSharedLibraryPath(name)
		if err := ort.InitializeEnvironment(ort.WithLogLevelError()); err != nil {
			envErr = core.Errorf(core.CodeInternal, "cannot initialise onnxruntime").WithCause(err)
			return
		}
		// Two builds of onnxruntime in one process would be two thread pools,
		// two allocators and two sets of global state that each assume they
		// are alone. The version check is the cheap way to know it is one.
		if got, want := ort.GetVersion(), sonnx.GetOnnxruntimeVersion(); !sameVersion(got, want) {
			envErr = core.Errorf(core.CodeInternal,
				"onnxruntime loaded for the diarizer is %s, but sherpa-onnx runs %s", got, want)
		}
	})
	return envErr
}

// sameVersion compares the version strings the two bindings report, which do
// not agree on decoration: one may carry a leading "v" or trailing build text.
func sameVersion(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "v"))
		if i := strings.IndexAny(s, " +-"); i >= 0 {
			s = s[:i]
		}
		return s
	}
	return norm(a) != "" && norm(a) == norm(b)
}
