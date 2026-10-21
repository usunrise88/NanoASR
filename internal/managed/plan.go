package managed

import (
	"runtime"
	"slices"
	"time"

	"github.com/usunrise88/nanoasr/internal/config"
)

// Features, in the order they are reported.
const (
	FeatureVAD           = "vad"
	FeatureTranscription = "transcription"
	FeatureRealtime      = "realtime"
	FeatureDiarization   = "diarization"
	FeatureIsolation     = "isolation"
)

var featureOrder = []string{
	FeatureVAD, FeatureTranscription, FeatureRealtime, FeatureDiarization, FeatureIsolation,
}

// Feature states.
const (
	// StateOff is a feature the settings do not ask for.
	StateOff = "off"
	// StateMissing is a feature asked for whose models are not all on disk.
	StateMissing = "missing"
	// StateReady is a feature this process serves.
	StateReady = "ready"
	// StateRestart is a feature the settings changed, or whose models arrived
	// after this process had to start without them: the next start serves it.
	StateRestart = "restart"
)

// Options is what a plan needs besides the settings.
type Options struct {
	Paths Paths
	// Token is the one key the instance accepts.
	Token string
	// UI mounts the test UI, which is otherwise left out.
	UI bool
	// Present reports whether a model is on disk.
	Present func(id string) bool
}

// Plan is the configuration one start runs with, and the models each feature
// was started with.
type Plan struct {
	Config config.Config
	// Started holds, per feature, the models this process was configured to
	// use; empty for a feature that is off or could not start.
	Started map[string][]string
}

// wants lists the models each feature needs under these settings.
func (s Settings) wants() map[string][]string {
	def := config.Default()
	w := map[string][]string{FeatureVAD: {def.VAD.Model}}
	if s.DefaultModel != "" {
		w[FeatureTranscription] = []string{s.DefaultModel}
	}
	if s.RealtimeModel != "" {
		w[FeatureRealtime] = []string{s.RealtimeModel}
	}
	if s.Diarization {
		w[FeatureDiarization] = []string{s.diarizationModel()}
	}
	if s.Isolation {
		w[FeatureIsolation] = []string{def.Isolation.Model, def.Isolation.EmbeddingModel}
	}
	return w
}

func (s Settings) diarizationModel() string {
	if s.DiarizationModel != "" {
		return s.DiarizationModel
	}
	return config.Default().Diarization.Model
}

// NewPlan turns settings into the configuration of one start.
//
// A feature whose models are not on disk is still configured when it finds
// them lazily, so it starts working the moment they arrive. The VAD and the
// streaming model cannot wait: they are left out of this start, and the
// status reports a restart once they are there.
func NewPlan(s Settings, o Options) (Plan, error) {
	cfg := config.Default()
	present := func(ids []string) bool {
		for _, id := range ids {
			if !o.Present(id) {
				return false
			}
		}
		return true
	}
	wants := s.wants()
	started := map[string][]string{}

	cfg.Server.Addr = "127.0.0.1:0"
	// A child that lingers keeps its parent's next start waiting on the lock;
	// queued jobs survive a short grace in the database anyway.
	cfg.Server.ShutdownGrace = config.Dur(5 * time.Second)
	cfg.Auth = config.Auth{Mode: "apikey", Keys: []config.APIKey{{
		Name: "app", Key: o.Token, Admin: true, Priority: config.PriorityInteractive,
	}}}
	cfg.API.Dialects = []string{config.DialectNative, config.DialectOpenAI}
	cfg.UI.Enabled = o.UI
	cfg.Audio.FFmpegPath = s.FFmpegPath

	cfg.ASR.ModelsDir = o.Paths.Models
	cfg.ASR.DefaultModel = s.DefaultModel
	started[FeatureTranscription] = wants[FeatureTranscription]
	threads := s.Threads
	if threads == 0 {
		// A quarter of the machine: the application this runs beside is
		// the one the user is looking at.
		threads = min(max(runtime.NumCPU()/4, 1), 4)
	}
	cfg.ASR.NumThreads = threads
	cfg.Isolation.NumThreads = threads
	cfg.ASR.MaxResidentModels = 1
	cfg.Jobs.MaxConcurrent = max(s.MaxConcurrent, 1)
	cfg.ASR.InferenceSlots = cfg.Jobs.MaxConcurrent

	if present(wants[FeatureVAD]) {
		started[FeatureVAD] = wants[FeatureVAD]
	} else {
		cfg.VAD.Enabled = false
	}

	if ids := wants[FeatureRealtime]; len(ids) > 0 && present(ids) {
		cfg.Realtime.Model = s.RealtimeModel
		cfg.API.Dialects = append(cfg.API.Dialects, config.DialectRealtime)
		started[FeatureRealtime] = ids
	}

	if s.Diarization {
		cfg.Diarization.Enabled = true
		cfg.Diarization.Backend = config.DiarizationSortformer
		cfg.Diarization.Model = s.diarizationModel()
		started[FeatureDiarization] = wants[FeatureDiarization]
	}
	if s.Isolation {
		cfg.Isolation.Enabled = true
		started[FeatureIsolation] = wants[FeatureIsolation]
	}

	cfg.Registry.AllowDownload = true
	cfg.Registry.AutoDownload = false
	cfg.Registry.Mirrors = s.Mirrors

	cfg.Storage.DBPath = o.Paths.DB
	cfg.Storage.TempDir = o.Paths.Spool
	if s.HistoryTTL != "" {
		d, err := time.ParseDuration(s.HistoryTTL)
		if err != nil {
			return Plan{}, err
		}
		cfg.Jobs.HistoryTTL = config.Dur(d)
	}
	if s.LogLevel != "" {
		cfg.Log.Level = s.LogLevel
	}

	cfg.Autotune()
	if err := cfg.Validate(); err != nil {
		return Plan{}, err
	}
	return Plan{Config: cfg, Started: started}, nil
}

// Feature is one feature as the status reports it.
type Feature struct {
	State string `json:"state"`
	// Models are the models the settings need for it.
	Models []string `json:"models,omitempty"`
	// Missing are those of them not on disk.
	Missing []string `json:"missing,omitempty"`
}

// features compares what the settings want with what this process started
// with and what is on disk now.
func features(s Settings, started map[string][]string, present func(string) bool) map[string]Feature {
	wants := s.wants()
	out := map[string]Feature{}
	for _, name := range featureOrder {
		ids := wants[name]
		f := Feature{Models: ids}
		for _, id := range ids {
			if !present(id) {
				f.Missing = append(f.Missing, id)
			}
		}
		running := started[name]
		switch {
		case len(ids) == 0 && len(running) == 0:
			f.State = StateOff
		case len(ids) == 0:
			// Turned off: still running until the restart.
			f.State = StateRestart
		case len(f.Missing) > 0:
			f.State = StateMissing
		case slices.Equal(ids, running):
			// A lazy feature was started with its models even when they were
			// not there yet, so one downloaded since is ready without a restart.
			f.State = StateReady
		default:
			f.State = StateRestart
		}
		out[name] = f
	}
	return out
}
