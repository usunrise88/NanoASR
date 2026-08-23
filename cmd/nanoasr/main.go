// Command nanoasr is the NanoASR server.
//
//	nanoasr serve   [-config path]   run the HTTP server
//	nanoasr models  [list|pull id]   inspect and fetch models
//	nanoasr version                  print versions, including the native libs
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	_ "github.com/usunrise88/nanoasr/internal/api/native"
	_ "github.com/usunrise88/nanoasr/internal/api/openai"
	"github.com/usunrise88/nanoasr/internal/asr/sherpa"
	"github.com/usunrise88/nanoasr/internal/config"
	"github.com/usunrise88/nanoasr/internal/httpx"
	"github.com/usunrise88/nanoasr/internal/registry"
	"github.com/usunrise88/nanoasr/internal/ui"
)

// version is stamped by the linker: -ldflags "-X main.version=$(git describe)".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "models":
		err = models(os.Args[2:])
	case "version":
		printVersion()
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "nanoasr:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `nanoasr — offline speech recognition server

  nanoasr serve   [-config FILE] [-addr ADDR]
  nanoasr models  list | inspect DIR [--probe WAV]
  nanoasr version

Configuration precedence: flags > NANOASR_* env > config file > computed defaults.
`)
}

func printVersion() {
	so, ort := sherpa.Versions()
	fmt.Printf("nanoasr     %s\n", version)
	fmt.Printf("sherpa-onnx %s\n", so)
	fmt.Printf("onnxruntime %s\n", ort)
	fmt.Printf("families    %v\n", sherpa.Families())
	fmt.Printf("dialects    %v\n", adapter.Available())
	fmt.Printf("ui          %v\n", ui.Enabled)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", os.Getenv("NANOASR_CONFIG"), "path to nanoasr.yaml")
	addr := fs.String("addr", "", "listen address (overrides config)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	// Flags win over everything, so they are applied after Load.
	if *addr != "" {
		cfg.Server.Addr = *addr
		if err := cfg.Validate(); err != nil {
			return err
		}
	}

	log := newLogger(cfg.Log)
	so, ort := sherpa.Versions()
	log.Info("starting nanoasr",
		"version", version, "sherpa_onnx", so, "onnxruntime", ort,
		"addr", cfg.Server.Addr, "dialects", cfg.API.Dialects,
		"inference_slots", cfg.ASR.InferenceSlots,
		"num_threads", cfg.ASR.NumThreads,
		"max_concurrent", cfg.Jobs.MaxConcurrent,
		"max_resident_models", cfg.ASR.MaxResidentModels)

	// Sizing is derived from the host when the operator has not measured their
	// hardware yet, so say out loud what that implies before it becomes an OOM.
	if est := cfg.PeakMemoryEstimateMB(); est > 0 {
		log.Info("estimated peak memory", "mb", est,
			"note", "resident models plus decoded PCM of every concurrent job")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv, err := build(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer srv.Close()
	srv.preload(ctx, cfg.ASR.DefaultModel, log)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":%q,"sherpa_onnx":%q,"onnxruntime":%q}`, version, so, ort)
	})
	// TODO(M3): readiness should also report queue depth once the queue exists.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if _, err := srv.models.List(r.Context()); err != nil {
			http.Error(w, `{"status":"degraded"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ready"}`)
	})

	if err := adapter.MountAll(mux, cfg.API.Dialects, srv.service, adapter.Deps{
		Models:         srv.models,
		MaxUploadBytes: cfg.Server.MaxUploadBytes,
	}); err != nil {
		return err
	}

	if cfg.UI.Enabled {
		h, err := ui.Handler(cfg.UI.Path)
		if err != nil {
			log.Warn("ui not mounted", "err", err)
		} else {
			mux.Handle(cfg.UI.Path+"/", h)
			log.Info("ui mounted", "path", cfg.UI.Path, "auth_required", cfg.UI.RequireAuth)
		}
	}

	// Auth goes last so that a 401 is still logged and still carries the
	// security headers, and so the request id is available to report.
	mw := []httpx.Middleware{
		httpx.WithRequestID(),
		httpx.Recover(log),
		httpx.AccessLog(log),
		httpx.SecurityHeaders(),
		httpx.LimitBody(cfg.Server.MaxUploadBytes),
	}

	if cfg.Auth.Mode == "open" {
		log.Warn("authentication disabled", "addr", cfg.Server.Addr,
			"note", "open mode is only permitted on a loopback address")
	} else {
		keys, err := httpx.NewStaticKeyStore(keySpecs(cfg.Auth.Keys))
		if err != nil {
			return err
		}
		// Health probes cannot present a credential, and a browser does not
		// send a bearer token when loading a script tag. Everything else is
		// authenticated.
		mw = append(mw, httpx.Auth(keys, "/healthz", "/readyz", cfg.UI.Path))

		// The key store now holds the digests, so drop the plaintext from the
		// configuration that /api/v1/config will eventually serve.
		cfg.Auth.Redact()
		log.Info("authentication enabled", "keys", keys.Names(),
			"public", []string{"/healthz", "/readyz", cfg.UI.Path})
	}

	httpSrv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           httpx.Chain(mux, mw...),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Duration,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down", "grace", cfg.Server.ShutdownGrace)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownGrace.Duration)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

func models(args []string) error {
	sub, args := takePositional(args, "list")

	switch sub {
	case "inspect":
		return inspectModel(args)
	case probeChildCommand:
		// Hidden: one candidate of `models inspect --probe`, run in its own
		// process so a dimension mismatch cannot abort the parent.
		return probeChild(args)
	}

	fs := flag.NewFlagSet("models "+sub, flag.ExitOnError)
	cfgPath := fs.String("config", os.Getenv("NANOASR_CONFIG"), "path to nanoasr.yaml")
	ids, err := parseFlags(fs, args)
	if err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	switch sub {
	case "list":
	case "pull":
		return pullModels(cfg, ids)
	case "catalog":
		return showCatalog(cfg)
	default:
		return fmt.Errorf("models %s: unknown subcommand; use list, pull, catalog or inspect", sub)
	}

	reg, err := registry.NewLocal(cfg.ASR.ModelsDir)
	if err != nil {
		return err
	}

	found, err := reg.Local(context.Background())
	if err != nil {
		return err
	}
	if len(found) == 0 {
		fmt.Printf("no models in %s\n", cfg.ASR.ModelsDir)
		return nil
	}

	fmt.Printf("%-24s %-14s %-12s %-10s %s\n", "ID", "FAMILY", "KIND", "LANGS", "REVISION")
	for _, m := range found {
		fmt.Printf("%-24s %-14s %-12s %-10s %s\n",
			m.ID, m.Family, m.EffectiveKind(), strings.Join(m.Languages, ","), m.Revision)
	}
	for _, p := range reg.Problems() {
		fmt.Fprintf(os.Stderr, "warning: %s\n", p)
	}
	return nil
}

// takePositional pulls a leading non-flag argument off the list.
func takePositional(args []string, fallback string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return fallback, args
}

// parseFlags parses fs from args given in any order and returns the positional
// arguments.
//
// The standard flag package stops at the first positional argument, so
// "models pull some-model -config x" leaves -config unparsed and silently uses
// the default configuration. Nothing warns; the command just does the wrong
// thing. Rather than teach every user that flags come first, the arguments are
// partitioned before parsing.
//
// Whether a flag consumes the next token is asked of the flag set itself, so a
// boolean flag does not swallow a positional argument that follows it.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}

		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if before, _, found := strings.Cut(name, "="); found {
			_ = before
			continue // the value travels with the flag
		}
		if takesValue(fs, name) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}

	if err := fs.Parse(flags); err != nil {
		return nil, err
	}
	return positional, nil
}

// takesValue reports whether a flag needs the following token. Boolean flags
// do not, and treating one as if it did would eat a positional argument.
func takesValue(fs *flag.FlagSet, name string) bool {
	f := fs.Lookup(name)
	if f == nil {
		return false // unknown: let flag.Parse produce the real complaint
	}
	boolFlag, ok := f.Value.(interface{ IsBoolFlag() bool })
	return !ok || !boolFlag.IsBoolFlag()
}

// keySpecs adapts configured keys to what the key store takes. The two types
// stay separate so internal/httpx does not depend on the configuration schema.
func keySpecs(keys []config.APIKey) []httpx.KeySpec {
	out := make([]httpx.KeySpec, 0, len(keys))
	for _, k := range keys {
		out = append(out, httpx.KeySpec{Name: k.Name, Secret: k.Key, Admin: k.Admin})
	}
	return out
}

func newLogger(c config.Log) *slog.Logger {
	level := slog.LevelInfo
	switch c.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}
