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
	"syscall"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	_ "github.com/usunrise88/nanoasr/internal/api/native"
	_ "github.com/usunrise88/nanoasr/internal/api/openai"
	"github.com/usunrise88/nanoasr/internal/asr/sherpa"
	"github.com/usunrise88/nanoasr/internal/config"
	"github.com/usunrise88/nanoasr/internal/httpx"
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
  nanoasr models  list | pull ID
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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":%q,"sherpa_onnx":%q,"onnxruntime":%q}`, version, so, ort)
	})
	// TODO(M3): readyz should report the queue and the pool, not just liveness.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// TODO(M1..M3): build the real service (registry → pool → pipeline → queue)
	// and pass it here instead of nil.
	if err := adapter.MountAll(mux, cfg.API.Dialects, nil, adapter.Deps{
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

	mw := []httpx.Middleware{
		httpx.WithRequestID(),
		httpx.Recover(log),
		httpx.AccessLog(log),
		httpx.SecurityHeaders(),
		httpx.LimitBody(cfg.Server.MaxUploadBytes),
	}
	// TODO(M3): insert httpx.Auth(keyStore) when cfg.Auth.Mode == "apikey".
	if cfg.Auth.Mode == "open" {
		log.Warn("authentication disabled", "addr", cfg.Server.Addr,
			"note", "open mode is only permitted on a loopback address")
	}

	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           httpx.Chain(mux, mw...),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down", "grace", cfg.Server.ShutdownGrace)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownGrace)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// TODO(M2): implement against the registry once the catalog is populated.
func models(args []string) error {
	fs := flag.NewFlagSet("models", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return fmt.Errorf("models: not implemented (milestone M2)")
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
