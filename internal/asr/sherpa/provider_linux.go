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

// probeCUDAProvider asks the dynamic loader to load the CUDA execution
// provider, and reports what it said.
//
// This is the whole check, and it is delegated rather than reimplemented on
// purpose: that library needs eight CUDA and cuDNN shared objects whose
// sonames change with the CUDA major version, and the loader already knows how
// to find them and which one is missing. onnxruntime performs this same dlopen
// when the provider is first used; doing it at startup moves the failure from
// the first request to the boot sequence, where somebody is watching.
func probeCUDAProvider(dir string) (path, loadErr string) {
	if dir == "" {
		return "", ""
	}
	path = filepath.Join(dir, "libonnxruntime_providers_cuda.so")
	if _, err := os.Stat(path); err != nil {
		return "", ""
	}

	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))

	// RTLD_LOCAL so nothing this library exports can shadow a symbol the rest
	// of the process resolves later; the handle is closed either way, and
	// onnxruntime opens its own.
	h := C.dlopen(cpath, C.RTLD_LAZY|C.RTLD_LOCAL)
	if h == nil {
		return path, strings.TrimSpace(C.GoString(C.dlerror()))
	}
	C.dlclose(h)
	return path, ""
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
