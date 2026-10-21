// Package ortenv is the one onnxruntime environment of the process, for the
// engines that run ONNX graphs directly rather than through sherpa-onnx: the
// Sortformer diarizer and the Voxlock voice isolator.
//
// sherpa-onnx already carries the onnxruntime library, and this package
// loads that same copy rather than shipping a second one, so the process keeps
// one onnxruntime however many engines use it.
package ortenv

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
	case "darwin":
		// macOS links the dylib by its @rpath install name, which a bare
		// dlopen does not search for. The path of the copy already loaded
		// does match it, and dlopen hands back that same image.
		paths, err := loadedRuntime()
		if err != nil {
			return "", err
		}
		if len(paths) != 1 {
			return "", fmt.Errorf("expected one onnxruntime in the process, found %d: %v", len(paths), paths)
		}
		return paths[0], nil
	default:
		return "", fmt.Errorf("onnxruntime engines are not supported on %s", runtime.GOOS)
	}
}

// Supported reports whether this platform can run the direct engines at all,
// so a constructor can refuse at startup rather than on first use.
func Supported() error {
	_, err := runtimeLibrary()
	return err
}

var (
	envOnce sync.Once
	envErr  error
)

// Ensure brings up the onnxruntime environment once per process.
//
// It is never torn down. Engines come and go — every test builds several — but
// the environment belongs to the process, and destroying it under a live
// session is undefined behaviour for no gain: the library stays mapped either
// way, because sherpa-onnx holds it.
//
// A failure is remembered rather than retried: every way this can fail is a
// property of the build or the host, and none of them fixes itself.
func Ensure() error {
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
		if got, want := ort.GetVersion(), sonnx.GetOnnxruntimeVersion(); !SameVersion(got, want) {
			envErr = core.Errorf(core.CodeInternal,
				"onnxruntime loaded for the direct engines is %s, but sherpa-onnx runs %s", got, want)
		}
	})
	return envErr
}

// SameVersion compares the version strings the two bindings report, which do
// not agree on decoration: one may carry a leading "v" or trailing build text.
func SameVersion(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "v"))
		if i := strings.IndexAny(s, " +-"); i >= 0 {
			s = s[:i]
		}
		return s
	}
	return norm(a) != "" && norm(a) == norm(b)
}

// RuntimeInfo reports the onnxruntime this package runs on: its version and
// the file it was loaded from. `nanoasr version` prints both, and the release
// job checks that the file is the one the archive ships — the check that the
// bare-name load above found sherpa's copy and not, on Windows, System32's.
func RuntimeInfo() (version, path string, err error) {
	if err := Ensure(); err != nil {
		return "", "", err
	}
	paths, err := loadedRuntime()
	if err != nil {
		return "", "", err
	}
	if len(paths) != 1 {
		return "", "", fmt.Errorf("expected one onnxruntime in the process, found %d: %v", len(paths), paths)
	}
	return ort.GetVersion(), paths[0], nil
}

// Tuning is what every session gets.
type Tuning struct {
	// IntraOpThreads is the thread budget of one Run. The caller reserves
	// exactly this many slots from the pipeline governor around the Run.
	IntraOpThreads int
	// NoSpinning stops idle onnxruntime workers from busy-waiting between
	// kernels. Spinning threads burn CPU outside the governor's accounting,
	// which is CPU sherpa's recognizers were counting on.
	NoSpinning bool
}

// SessionOptions builds the options every direct session is created with.
// The caller destroys them once its sessions exist.
func SessionOptions(tune Tuning) (*ort.SessionOptions, error) {
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "cannot create onnxruntime session options").WithCause(err)
	}
	threads := tune.IntraOpThreads
	if threads < 1 {
		threads = 1
	}
	steps := []error{
		opts.SetIntraOpNumThreads(threads),
		// One graph, one Run at a time per call: inter-op parallelism would
		// only add threads the governor does not know about.
		opts.SetInterOpNumThreads(1),
		opts.SetExecutionMode(ort.ExecutionModeSequential),
	}
	if tune.NoSpinning {
		steps = append(steps, opts.AddSessionConfigEntry("session.intra_op.allow_spinning", "0"))
	}
	for _, e := range steps {
		if e != nil {
			_ = opts.Destroy()
			return nil, core.Errorf(core.CodeInternal, "cannot configure onnxruntime session").WithCause(e)
		}
	}
	return opts, nil
}
