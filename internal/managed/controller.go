package managed

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"sync"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Status is what GET /api/v1/managed answers: what the application chose,
// what this process serves, and what is still to do.
type Status struct {
	Version    string `json:"version"`
	APIVersion int    `json:"api_version"`
	// Configured is false until the application saves settings for the
	// first time; the first start of an instance is its setup.
	Configured bool `json:"configured"`
	// RestartRequired says the next start would serve something this one
	// does not: a setting changed, or a model arrived that only a start
	// loads.
	RestartRequired bool               `json:"restart_required"`
	DataDir         string             `json:"data_dir"`
	ModelsDir       string             `json:"models_dir"`
	Settings        Settings           `json:"settings"`
	Features        map[string]Feature `json:"features"`
	// MissingModels is every model the settings need that is not on disk:
	// what the application has to download.
	MissingModels []string `json:"missing_models"`
}

// Controller holds the settings of a running instance and serves them.
type Controller struct {
	version string
	paths   Paths
	started map[string][]string
	present func(string) bool
	lookup  Lookup
	events  *Events

	mu         sync.Mutex
	settings   Settings
	configured bool
	// announced is the restart last announced, so each change is said once.
	announced bool
}

// ControllerOptions builds a Controller.
type ControllerOptions struct {
	Version    string
	Paths      Paths
	Settings   Settings
	Configured bool
	// Started is the plan this process runs.
	Started map[string][]string
	Present func(id string) bool
	Lookup  Lookup
	Events  *Events
}

func NewController(o ControllerOptions) *Controller {
	return &Controller{
		version:    o.Version,
		paths:      o.Paths,
		started:    o.Started,
		present:    o.Present,
		lookup:     o.Lookup,
		events:     o.Events,
		settings:   o.Settings,
		configured: o.Configured,
	}
}

// Status reports the instance as it stands now.
func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusLocked()
}

func (c *Controller) statusLocked() Status {
	fs := features(c.settings, c.started, c.present)
	st := Status{
		Version:       c.version,
		APIVersion:    APIVersion,
		Configured:    c.configured,
		DataDir:       c.paths.Data,
		ModelsDir:     c.paths.Models,
		Settings:      c.settings,
		Features:      fs,
		MissingModels: []string{},
	}
	if st.Settings.Mirrors == nil {
		// A list in JSON either way, so a reader need not handle null.
		st.Settings.Mirrors = []string{}
	}
	for _, name := range featureOrder {
		f := fs[name]
		if f.State == StateRestart {
			st.RestartRequired = true
		}
		for _, id := range f.Missing {
			if !slices.Contains(st.MissingModels, id) {
				st.MissingModels = append(st.MissingModels, id)
			}
		}
	}
	return st
}

// Changed is told that the models on disk changed — a download finished, a
// model was removed — and announces a restart the change made necessary.
func (c *Controller) Changed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.announceLocked(c.statusLocked())
}

func (c *Controller) announceLocked(st Status) {
	if st.RestartRequired == c.announced {
		return
	}
	c.announced = st.RestartRequired
	if !st.RestartRequired {
		return
	}
	var which []string
	for _, name := range featureOrder {
		if st.Features[name].State == StateRestart {
			which = append(which, name)
		}
	}
	c.events.Emit(EventRestartRequired, map[string]any{"features": which})
}

// maxSettingsBody bounds a settings request; the settings are a few hundred
// bytes.
const maxSettingsBody = 64 << 10

// Mount adds the managed endpoints. writeError renders a refusal in the API's
// own error shape.
//
//	GET   /api/v1/managed           the status
//	PATCH /api/v1/managed/settings  change and save settings; answers the status
func (c *Controller) Mount(mux *http.ServeMux, writeError func(http.ResponseWriter, *http.Request, error)) {
	mux.HandleFunc("GET /api/v1/managed", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, c.Status())
	})
	mux.HandleFunc("PATCH /api/v1/managed/settings", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxSettingsBody+1))
		if err != nil {
			writeError(w, r, core.Errorf(core.CodeInvalidRequest, "cannot read the request body"))
			return
		}
		if len(body) > maxSettingsBody {
			writeError(w, r, core.Errorf(core.CodeInvalidRequest, "the settings are too large"))
			return
		}
		st, err := c.Update(body)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, st)
	})
}

// Update applies a settings patch, saves it, and reports the new status. The
// running process keeps what it started with; the status says what the next
// start changes.
func (c *Controller) Update(patch []byte) (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	next, err := c.settings.Patch(patch)
	if err != nil {
		return Status{}, err
	}
	if err := next.Validate(c.lookup); err != nil {
		return Status{}, err
	}
	if err := SaveSettings(c.paths.Settings, next); err != nil {
		return Status{}, core.Errorf(core.CodeInternal, "cannot save the settings").WithCause(err)
	}
	c.settings, c.configured = next, true
	st := c.statusLocked()
	c.announceLocked(st)
	return st, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
