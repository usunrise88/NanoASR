//go:build linux

package sherpa

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingLibrariesNamesEveryOneInOrder(t *testing.T) {
	present := map[string]bool{"libc.so.6": true, "libcudart.so.12": true}
	open := func(name string) error {
		if present[name] {
			return nil
		}
		return errors.New("cannot open shared object file")
	}

	got := missingLibraries(
		[]string{"libcublasLt.so.12", "libc.so.6", "libcudnn.so.9", "libcudart.so.12"},
		open)

	want := []string{"libcublasLt.so.12", "libcudnn.so.9"}
	if len(got) != len(want) {
		t.Fatalf("missing = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("missing = %v, want %v", got, want)
		}
	}
}

func TestMissingLibrariesSaysNothingWhenAllResolve(t *testing.T) {
	if got := missingLibraries([]string{"libc.so.6"}, func(string) error { return nil }); got != nil {
		t.Fatalf("missing = %v, want none", got)
	}
}

// TestProbeAcceptsAProviderThatIsNotLoadableAlone is the regression test for
// the check this package shipped first, which asked the loader to load
// libonnxruntime_providers_cuda.so on its own. That library imports
// Provider_GetHost from libonnxruntime_providers_shared.so without declaring a
// dependency on it, and is linked BIND_NOW, so loading it alone fails with an
// undefined symbol on every machine however well CUDA is installed — and the
// server refused to start on a host where the GPU was perfectly usable.
//
// The stand-in has the same shape: a symbol resolved from a library it does not
// depend on, BIND_NOW, and a real dependency that is present.
func TestProbeAcceptsAProviderThatIsNotLoadableAlone(t *testing.T) {
	cc := compiler(t)
	dir := t.TempDir()

	build(t, cc, dir, "libnanoasr_probe_bridge.so",
		"void nanoasr_probe_host(void) {}\n",
		"-Wl,-soname,libnanoasr_probe_bridge.so")

	// No link against the bridge: the symbol stays undefined, as in the real
	// provider. -z now is what makes RTLD_LAZY unable to defer it.
	build(t, cc, dir, "libonnxruntime_providers_cuda.so",
		"extern void nanoasr_probe_host(void);\nvoid nanoasr_probe_use(void) { nanoasr_probe_host(); }\n",
		"-Wl,-z,now", "-lm")

	path, loadErr := probeCUDAProvider(dir)
	if path == "" {
		t.Fatal("probe found no provider library in the directory it was given")
	}
	if loadErr != "" {
		t.Fatalf("provider reported unusable, but every library it imports is present: %s", loadErr)
	}
}

func TestProbeNamesAnAbsentDependency(t *testing.T) {
	cc := compiler(t)
	dir := t.TempDir()

	// Built to link against, then removed: the DT_NEEDED entry outlives it,
	// which is the state of a host with no CUDA installed.
	stub := build(t, cc, dir, "libnanoasr_absent.so.1", "void nanoasr_gone(void) {}\n",
		"-Wl,-soname,libnanoasr_absent.so.1")
	build(t, cc, dir, "libonnxruntime_providers_cuda.so",
		"extern void nanoasr_gone(void);\nvoid nanoasr_probe_use(void) { nanoasr_gone(); }\n",
		"-L"+dir, "-l:libnanoasr_absent.so.1")
	if err := os.Remove(stub); err != nil {
		t.Fatal(err)
	}

	_, loadErr := probeCUDAProvider(dir)
	if !strings.Contains(loadErr, "libnanoasr_absent.so.1") {
		t.Fatalf("loadErr = %q, want it to name libnanoasr_absent.so.1", loadErr)
	}
}

func TestProbeIsSilentWithoutAProviderLibrary(t *testing.T) {
	path, loadErr := probeCUDAProvider(t.TempDir())
	if path != "" || loadErr != "" {
		t.Fatalf("probe = (%q, %q), want empty for a CPU-only build", path, loadErr)
	}
}

func compiler(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"cc", "gcc", "clang"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("no C compiler to build the stand-in libraries with")
	return ""
}

// build compiles src into dir/name as a shared library and returns its path.
func build(t *testing.T, cc, dir, name, src string, extra ...string) string {
	t.Helper()
	c := filepath.Join(dir, name+".c")
	if err := os.WriteFile(c, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, name)
	args := append([]string{"-shared", "-fPIC", "-o", out, c}, extra...)
	if cmd := exec.Command(cc, args...); true {
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %s: %v\n%s", cc, strings.Join(args, " "), err, b)
		}
	}
	return out
}
