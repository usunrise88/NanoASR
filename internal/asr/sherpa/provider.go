package sherpa

import (
	"fmt"
	"strings"
)

// Execution providers this build understands. The strings are onnxruntime's,
// passed straight through to sherpa-onnx.
const (
	ProviderCPU  = "cpu"
	ProviderCUDA = "cuda"
)

// Runtime describes the native libraries this process actually loaded and what
// they are able to do.
//
// Every field is measured rather than configured. The reason is onnxruntime's
// behaviour when it is asked for a provider it does not have: it logs a line
// nobody reads and runs on the CPU. A server that was told to use the GPU and
// is quietly four times slower than expected is a worse outcome than one that
// refuses to start, so the facts are gathered here and acted on at startup.
type Runtime struct {
	SherpaOnnx  string
	OnnxRuntime string
	// LibraryDir is the directory the loaded onnxruntime came from. It is also
	// where onnxruntime looks for its execution providers, which is why it is
	// the one path that matters: swapping the GPU build in means putting it
	// earlier in the loader's search order, not changing a setting.
	LibraryDir string
	// Providers are the provider shared objects found beside it, by bare name.
	Providers []string
	// CUDAProviderPath is the CUDA execution provider library, empty when the
	// loaded onnxruntime is a CPU-only build.
	CUDAProviderPath string
	// CUDAProviderError is what the dynamic loader said when asked to load
	// that library. Empty means it loaded, which means every CUDA and cuDNN
	// library it needs resolved too — the check is delegated to the loader
	// rather than reimplemented against a list of sonames that changes with
	// every CUDA major version.
	CUDAProviderError string
	// Driver is the NVIDIA kernel driver version, empty when the driver is not
	// loaded or not visible to this process.
	Driver string
	// Devices are the NVIDIA device nodes this process can see.
	Devices []string
	// Platform is false where the GPU runtime is not something this build can
	// inspect. Nothing is refused on a guess: see CheckProvider.
	Platform bool
}

// Probe gathers the runtime facts. It is cheap except for one dlopen of the
// CUDA provider library, which is the same work onnxruntime would do later.
func Probe() Runtime {
	rt := Runtime{Platform: probeSupported}
	rt.SherpaOnnx, rt.OnnxRuntime = Versions()
	rt.LibraryDir = probeLibraryDir()
	rt.Providers = probeProviders(rt.LibraryDir)
	rt.CUDAProviderPath, rt.CUDAProviderError = probeCUDAProvider(rt.LibraryDir)
	rt.Driver, rt.Devices = probeDevices()
	return rt
}

// CUDAReady reports whether a CUDA session could be created right now.
func (rt Runtime) CUDAReady() bool {
	return rt.CUDAProviderPath != "" && rt.CUDAProviderError == ""
}

// Summary is one line for the startup log.
func (rt Runtime) Summary() string {
	if !rt.Platform {
		return "provider detection is not available on this platform"
	}
	if rt.LibraryDir == "" {
		return "onnxruntime library directory could not be determined"
	}
	switch {
	case rt.CUDAReady() && len(rt.Devices) > 0:
		return fmt.Sprintf("cuda ready (driver %s, %d device(s))", rt.driverOrUnknown(), len(rt.Devices))
	case rt.CUDAReady():
		return "cuda libraries present but no device is visible"
	case rt.CUDAProviderPath != "":
		return "cuda provider present but unloadable: " + rt.CUDAProviderError
	default:
		return "cpu-only onnxruntime build"
	}
}

func (rt Runtime) driverOrUnknown() string {
	if rt.Driver == "" {
		return "unknown"
	}
	return rt.Driver
}

// CheckProvider decides whether the configured provider can be served.
//
// It returns warnings for what is suspicious and an error for what is settled.
// The split matters: a missing provider library is a fact — onnxruntime cannot
// create a CUDA session without it, so starting would be a lie — while a
// missing device node is only evidence, and a container runtime that injects
// the driver differently (WSL2 does) would make a refusal wrong.
func CheckProvider(provider string) (warnings []string, err error) {
	if provider == "" || provider == ProviderCPU {
		return nil, nil
	}
	if provider != ProviderCUDA {
		return nil, fmt.Errorf("unknown execution provider %q", provider)
	}

	rt := Probe()
	if !rt.Platform {
		return nil, fmt.Errorf(
			"asr.provider is %q, but the GPU runtime is only supported on linux/amd64 in this build; "+
				"set asr.provider: cpu", ProviderCUDA)
	}
	if rt.CUDAProviderPath == "" {
		return nil, fmt.Errorf(
			"asr.provider is %q but the loaded onnxruntime is a CPU-only build: "+
				"no libonnxruntime_providers_cuda.so beside %s.\n"+
				"Install the GPU libraries with scripts/install-gpu.sh, or use the "+
				"nanoasr:*-gpu image, or set asr.provider: cpu",
			ProviderCUDA, libraryDirOrUnknown(rt))
	}
	if rt.CUDAProviderError != "" {
		return nil, fmt.Errorf(
			"asr.provider is %q and %s is present, but the dynamic loader cannot load it: %s.\n"+
				"That library needs CUDA 12 (or 13, matching the archive you installed) and cuDNN 9 "+
				"on the library path; install them, or set asr.provider: cpu",
			ProviderCUDA, rt.CUDAProviderPath, rt.CUDAProviderError)
	}

	if len(rt.Devices) == 0 {
		warnings = append(warnings,
			"no NVIDIA device node is visible to this process: "+
				"a container needs --gpus all (or the device plugin's resource limit). "+
				"WSL2 exposes the driver without /dev/nvidia*, so this is only a warning")
	}
	if rt.Driver == "" {
		warnings = append(warnings,
			"the NVIDIA kernel driver version could not be read from /proc/driver/nvidia/version")
	}
	return warnings, nil
}

func libraryDirOrUnknown(rt Runtime) string {
	if rt.LibraryDir == "" {
		return "the loaded onnxruntime"
	}
	return rt.LibraryDir
}

// providerOrCPU is the normalising helper every caller uses, so that an unset
// provider means the CPU in exactly one place.
func providerOrCPU(p string) string {
	if p = strings.TrimSpace(p); p != "" {
		return p
	}
	return ProviderCPU
}
