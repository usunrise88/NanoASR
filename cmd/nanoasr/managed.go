package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	"github.com/usunrise88/nanoasr/internal/api/native"
	"github.com/usunrise88/nanoasr/internal/config"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/httpx"
	"github.com/usunrise88/nanoasr/internal/managed"
	"github.com/usunrise88/nanoasr/internal/registry"
)

// managedCommand runs NanoASR as the background process of an application:
// see internal/managed for the contract and nanoasr-embedded.md for the
// application's side of it.
//
// Every way it can fail to start is said on stdout as a fatal event before it
// returns, because the application reads that and nothing else.
func managedCommand(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	events := managed.NewEvents(stdout)
	fail := func(code string, err error) error {
		events.Fatal(code, err.Error())
		return err
	}

	fs := flag.NewFlagSet("managed", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dataDir := fs.String("data-dir", "", "where settings, models, the database and the log live (default: the user's application data directory)")
	parentPID := fs.Int("parent-pid", 0, "stop when this process exits")
	watchStdin := fs.Bool("watch-stdin", true, "stop when standard input closes")
	lowPriority := fs.Bool("low-priority", true, "run below normal priority")
	ui := fs.Bool("ui", false, "serve the test UI at /ui")
	if _, err := parseFlags(fs, args); err != nil {
		return fail("invalid_arguments", err)
	}

	managed.IgnoreBrokenPipe()

	dir := *dataDir
	if dir == "" {
		d, err := managed.DefaultDataDir()
		if err != nil {
			return fail("data_dir_unavailable", err)
		}
		dir = d
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return fail("data_dir_unavailable", err)
	}
	paths := managed.PathsFor(dir)
	if err := paths.Create(); err != nil {
		return fail("data_dir_unavailable", err)
	}
	lock, err := managed.Lock(paths.Lock)
	if errors.Is(err, managed.ErrLocked) {
		return fail("data_dir_locked", fmt.Errorf("%w: %s", err, dir))
	}
	if err != nil {
		return fail("data_dir_unavailable", err)
	}
	defer lock.Close()

	logw, closeLog, err := openLog(paths.Log)
	if err != nil {
		return fail("data_dir_unavailable", err)
	}
	defer closeLog()

	settings, configured, err := managed.LoadSettings(paths.Settings)
	if err != nil {
		// Unreadable settings must not keep the application from starting
		// its engine: it is set up again, and the old file is kept to look at.
		bad := paths.Settings + ".invalid"
		_ = os.Rename(paths.Settings, bad)
		events.Emit(managed.EventWarning, map[string]any{
			"code":    "settings_reset",
			"message": fmt.Sprintf("the settings could not be read and were moved to %s: %v", bad, err),
		})
		settings, configured = managed.Settings{}, false
	}

	token, err := config.NewKeySecret()
	if err != nil {
		return fail("internal", err)
	}
	local, err := registry.NewLocal(paths.Models)
	if err != nil {
		return fail("data_dir_unavailable", err)
	}
	plan, err := managed.NewPlan(settings, managed.Options{
		Paths: paths,
		Token: token,
		UI:    *ui,
		Present: func(id string) bool {
			_, err := local.Dir(id)
			return err == nil
		},
	})
	if err != nil {
		return fail("settings_invalid", err)
	}
	log := newLogger(plan.Config.Log, logw)

	if *lowPriority {
		if err := managed.LowerPriority(); err != nil {
			log.Warn("could not lower the process priority", "err", err)
		}
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	ctx, stopSignals := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	// Set before the cancels above run on the way out: the context ending
	// because this function returned — after a fatal event, say — is not a
	// stop anybody asked for, and must not be reported as one.
	var returned atomic.Bool
	defer returned.Store(true)
	stop := func(reason string) { cancel(errors.New(reason)) }
	if *watchStdin {
		managed.WatchStdin(stdin, stop)
	}
	if *parentPID > 0 {
		if err := managed.WatchParent(ctx, *parentPID, stop); err != nil {
			return fail("parent_gone", err)
		}
	}
	go func() {
		<-ctx.Done()
		if returned.Load() {
			return
		}
		reason := "signal"
		if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
			reason = cause.Error()
		}
		log.Info("stopping", "reason", reason)
		events.Emit(managed.EventStopping, map[string]any{"reason": reason})
	}()

	// The port is the system's choice: a fixed one is taken on some machine
	// sooner or later, and on the loopback interface no firewall asks.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fail("listen_failed", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	var ctrl *managed.Controller
	err = run(ctx, plan.Config, log, runOptions{
		listener:   ln,
		middleware: []httpx.Middleware{httpx.RequireHost(httpx.LoopbackHosts(strconv.Itoa(port))...)},
		models: func(m core.ModelService) core.ModelService {
			return notifyingModels{ModelService: m, changed: func() { ctrl.Changed() }}
		},
		mount: func(mux *http.ServeMux, srv *server) {
			ctrl = managed.NewController(managed.ControllerOptions{
				Version:    version,
				Paths:      paths,
				Settings:   settings,
				Configured: configured,
				Started:    plan.Started,
				Present: func(id string) bool {
					_, err := srv.registry.Dir(id)
					return err == nil
				},
				Lookup: func(id string) (registry.Manifest, error) {
					return srv.registry.Resolve(context.Background(), id)
				},
				Events: events,
			})
			ctrl.Mount(mux, native.WriteProblem)
		},
		routes: managedRoutes(),
		// The application waits for the ready event before it can do
		// anything at all; a model takes seconds to load on a slow machine.
		preloadInBackground: true,
		serving: func(addr net.Addr) {
			st := ctrl.Status()
			log.Info("managed instance ready", "addr", addr.String(), "data_dir", dir,
				"configured", st.Configured, "missing_models", st.MissingModels)
			events.Emit(managed.EventReady, map[string]any{
				"url":              "http://" + addr.String(),
				"port":             port,
				"token":            token,
				"pid":              os.Getpid(),
				"version":          version,
				"api_version":      managed.APIVersion,
				"data_dir":         dir,
				"configured":       st.Configured,
				"restart_required": st.RestartRequired,
				"missing_models":   st.MissingModels,
			})
		},
	})
	if err != nil {
		return fail("server_failed", err)
	}
	return nil
}

// notifyingModels tells the managed controller when the models on disk
// change, so it can announce a restart a download made necessary.
type notifyingModels struct {
	core.ModelService
	changed func()
}

func (m notifyingModels) Download(ctx context.Context, id string) (<-chan core.DownloadProgress, error) {
	in, err := m.ModelService.Download(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make(chan core.DownloadProgress, 16)
	go func() {
		defer close(out)
		for p := range in {
			if !p.Done {
				// Dropped for a reader that fell behind or went away, as the
				// registry does: a progress tick is replaced by the next.
				select {
				case out <- p:
				default:
				}
				continue
			}
			if p.Err == "" {
				// Before the last message: whoever acts on it then finds the
				// status already changed.
				m.changed()
			}
			select {
			case out <- p:
			case <-time.After(10 * time.Second):
			}
		}
	}()
	return out, nil
}

func (m notifyingModels) Delete(ctx context.Context, id string) error {
	err := m.ModelService.Delete(ctx, id)
	if err == nil {
		m.changed()
	}
	return err
}

func managedRoutes() []adapter.Route {
	return []adapter.Route{{
		Method: "GET", Path: "/api/v1/managed",
		Summary: "The managed instance: settings, features, missing models.",
		Detail: "Served by `nanoasr managed` only. restart_required says the next start " +
			"serves something this one does not.",
	}, {
		Method: "PATCH", Path: "/api/v1/managed/settings",
		Summary: "Change and save the managed instance's settings.",
		Detail: "Takes the fields to change; unknown fields are refused. Saved for every " +
			"later start, applied by the next one. Answers the status.",
	}}
}
