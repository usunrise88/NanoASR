package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/usunrise88/nanoasr/internal/asr/sherpa"
	"github.com/usunrise88/nanoasr/internal/config"
)

// gpuCommand reports what the native runtime can actually do.
//
// It exists because "the GPU is not being used" has four different causes that
// look identical from the outside — the CPU build of the libraries is loaded,
// the CUDA libraries are missing, the container has no device, or the
// configuration still says cpu — and each has a different fix. The server
// checks the same facts at startup and refuses to run on a mismatch; this
// prints them without needing a configuration, a model or a port.
func gpuCommand(args []string) error {
	fs := flag.NewFlagSet("gpu", flag.ExitOnError)
	cfgPath := fs.String("config", os.Getenv("NANOASR_CONFIG"), "path to nanoasr.yaml")
	want := fs.String("provider", "", "check this provider instead of the configured one")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}

	rt := sherpa.Probe()
	fmt.Printf("sherpa-onnx   %s\n", rt.SherpaOnnx)
	fmt.Printf("onnxruntime   %s\n", rt.OnnxRuntime)
	fmt.Printf("library dir   %s\n", orNone(rt.LibraryDir))
	fmt.Printf("providers     %s\n", orNone(strings.Join(rt.Providers, " ")))
	fmt.Printf("cuda provider %s\n", orNone(rt.CUDAProviderPath))
	if rt.CUDAProviderError != "" {
		fmt.Printf("cuda loader   %s\n", rt.CUDAProviderError)
	}
	fmt.Printf("nvidia driver %s\n", orNone(rt.Driver))
	fmt.Printf("devices       %s\n", orNone(strings.Join(rt.Devices, " ")))
	fmt.Printf("runtime       %s\n", rt.Summary())

	// The configuration is optional here on purpose: this command has to be
	// usable on a machine where the server does not start yet, which is when
	// it is needed most. A configuration that fails to load is reported and
	// stepped over rather than being fatal — an unrelated mistake in it, such
	// as apikey mode with no keys, must not hide the GPU diagnosis.
	provider, from := *want, "-provider"
	if provider == "" {
		provider, from = config.ProviderCPU, "default"
		cfg, err := config.Load(*cfgPath)
		switch {
		case err == nil:
			provider, from = cfg.ASR.Provider, configSource(*cfgPath)
		case os.Getenv("NANOASR_PROVIDER") != "":
			provider, from = os.Getenv("NANOASR_PROVIDER"), "NANOASR_PROVIDER"
			fmt.Fprintf(os.Stderr, "note: the configuration could not be loaded: %v\n", err)
		default:
			fmt.Fprintf(os.Stderr, "note: the configuration could not be loaded: %v\n", err)
			from = "default, the configuration is unreadable"
		}
	}
	fmt.Printf("provider      %s (from %s)\n", provider, from)

	warnings, err := sherpa.CheckProvider(provider)
	for _, w := range warnings {
		fmt.Printf("warning       %s\n", w)
	}
	if err != nil {
		// Reported as the command's failure, not just printed: this is what a
		// deployment script checks before it restarts the service.
		return err
	}
	fmt.Printf("verdict       %s can be served\n", provider)
	return nil
}

func configSource(path string) string {
	if path == "" {
		return "the built-in defaults and NANOASR_* environment"
	}
	return path
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
