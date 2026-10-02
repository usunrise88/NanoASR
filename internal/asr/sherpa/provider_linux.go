//go:build linux

package sherpa

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdlib.h>
*/
import "C"

import (
	"bufio"
	"debug/elf"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unsafe"
)

const probeSupported = true

// probeLibraryDir reports where the onnxruntime this process is running came
// from, by reading its own memory map.
//
// Asking the loader is the only honest way to answer it. The library is found
// through RUNPATH and LD_LIBRARY_PATH, both of which can put something other
// than the build-time path first — which is exactly how the GPU libraries are
// installed — so any path computed from the build would describe a library that
// may not be the one in use.
func probeLibraryDir() string {
	f, err := os.Open("/proc/self/maps")
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// A mapping line ends in the backing file's path. Everything before it —
	// address range, permissions, offset, device, inode — is hex, digits,
	// colons and dashes, so the first slash on the line starts the path even
	// when the path itself contains spaces.
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, '/')
		if i < 0 {
			continue
		}
		path := strings.TrimSuffix(strings.TrimSpace(line[i:]), " (deleted)")
		if strings.HasPrefix(filepath.Base(path), "libonnxruntime.so") {
			return filepath.Dir(path)
		}
	}
	return ""
}

// probeProviders lists the execution provider libraries beside onnxruntime.
// That directory is where onnxruntime itself looks for them.
func probeProviders(dir string) []string {
	if dir == "" {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(dir, "libonnxruntime_providers_*.so*"))
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, filepath.Base(m))
	}
	sort.Strings(out)
	return out
}

// probeCUDAProvider reports whether the CUDA execution provider's own
// dependencies are present, and names the ones that are not.
//
// What it deliberately does not do is load that library. The provider is not
// standalone: it imports Provider_GetHost from
// libonnxruntime_providers_shared.so and declares no dependency on it, because
// onnxruntime loads that bridge into the global symbol scope first and the
// provider second. It is also linked BIND_NOW, so RTLD_LAZY defers nothing.
// Loading it on its own therefore fails with "undefined symbol:
// Provider_GetHost" on every machine, however well CUDA is installed — which
// is exactly what the first version of this check did, and it refused the GPU
// on a correctly configured host.
//
// Loading the bridge first would resolve that symbol, but it would also run the
// provider's initialisers outside the sequence onnxruntime drives, and a
// startup probe that can crash the server is worse than one that answers a
// narrower question. So the question is narrowed to the one that was worth
// asking in the first place: are the CUDA and cuDNN libraries it needs on the
// library path? The list comes from the library's own dynamic section rather
// than a table here, so it stays right as sonames change with the CUDA major
// version, and the dependencies are leaf libraries that are safe to load.
//
// Whether those libraries can then run kernels on this particular device is
// not knowable without building a session, so it is left to the first model
// load, where sherpa-onnx will say so.
func probeCUDAProvider(dir string) (path, loadErr string) {
	if dir == "" {
		return "", ""
	}
	path = filepath.Join(dir, "libonnxruntime_providers_cuda.so")
	if _, err := os.Stat(path); err != nil {
		return "", ""
	}

	deps, err := importedLibraries(path)
	if err != nil {
		return path, "cannot read its dynamic section: " + err.Error()
	}
	if missing := missingLibraries(deps, dlopenByName); len(missing) > 0 {
		return path, "cannot find " + strings.Join(missing, ", ")
	}
	return path, ""
}

// importedLibraries is the DT_NEEDED list: what the loader will require.
func importedLibraries(path string) ([]string, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ImportedLibraries()
}

// missingLibraries reports which of names the loader cannot find, in the order
// they appear. open is a parameter so the decision can be tested without a
// machine that has CUDA on it or one that does not.
func missingLibraries(names []string, open func(string) error) []string {
	var missing []string
	for _, name := range names {
		if err := open(name); err != nil {
			missing = append(missing, name)
		}
	}
	return missing
}

// dlopenByName resolves a soname the way the loader will when it comes to load
// the provider: RUNPATH is absent from that library, so a plain soname lookup
// through LD_LIBRARY_PATH and the ld.so cache has the same answer.
func dlopenByName(soname string) error {
	cname := C.CString(soname)
	defer C.free(unsafe.Pointer(cname))

	// RTLD_LOCAL so nothing these libraries export can shadow a symbol the
	// rest of the process resolves later; onnxruntime opens its own handles.
	h := C.dlopen(cname, C.RTLD_LAZY|C.RTLD_LOCAL)
	if h == nil {
		if msg := strings.TrimSpace(C.GoString(C.dlerror())); msg != "" {
			return errors.New(msg)
		}
		return errors.New("dlopen failed")
	}
	C.dlclose(h)
	return nil
}

var driverVersion = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

// probeDevices reports the NVIDIA driver version and the device nodes visible
// to this process. Both are absent in a container started without --gpus, which
// is the most common reason a GPU build runs on the CPU.
func probeDevices() (driver string, devices []string) {
	if b, err := os.ReadFile("/proc/driver/nvidia/version"); err == nil {
		// "NVRM version: NVIDIA UNIX x86_64 Kernel Module  550.54.14  ..."
		line, _, _ := strings.Cut(string(b), "\n")
		driver = driverVersion.FindString(line)
	}
	if m, err := filepath.Glob("/dev/nvidia[0-9]*"); err == nil {
		sort.Strings(m)
		devices = m
	}
	return driver, devices
}
