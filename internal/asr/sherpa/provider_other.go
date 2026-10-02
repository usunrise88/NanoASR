//go:build !linux

package sherpa

// The GPU runtime NanoASR ships is a linux/amd64 archive of sherpa-onnx built
// against a CUDA onnxruntime, and the checks in provider_linux.go read
// /proc/self/maps to find out what was actually loaded. Neither exists here, so
// nothing is detected and CheckProvider refuses "cuda" outright rather than
// letting onnxruntime fall back to the CPU in silence.
const probeSupported = false

func probeLibraryDir() string                                { return "" }
func probeProviders(string) []string                         { return nil }
func probeCUDAProvider(string) (path string, loadErr string) { return "", "" }
func probeDevices() (driver string, devices []string)        { return "", nil }
