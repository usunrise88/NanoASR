package managed

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"runtime"
	"time"

	"github.com/usunrise88/nanoasr/internal/config"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/registry"
)

// Settings is what the application chose. It is the whole of what a managed
// instance can be told: the rest of the configuration is either fixed by
// running managed (the address, the keys, the paths) or derived from these.
//
// A zero value is an instance nobody has set up yet: no model to transcribe
// with, nothing streaming, nothing extra.
type Settings struct {
	// DefaultModel transcribes a request that names no model.
	DefaultModel string `json:"default_model"`
	// RealtimeModel is the streaming model behind /v1/realtime. Empty turns
	// streaming recognition off.
	RealtimeModel string `json:"realtime_model"`
	// Diarization tells the speakers apart on request (diarize=true).
	Diarization bool `json:"diarization"`
	// DiarizationModel overrides the diarizer; empty is the default one.
	DiarizationModel string `json:"diarization_model,omitempty"`
	// Isolation extracts one voice from a recording. It needs Diarization.
	Isolation bool `json:"isolation"`

	// Provider is where the models run: "cpu", or "coreml" on a Mac, which
	// runs the CoreML build of every chosen model that has one on the Neural
	// Engine. Empty is the CPU.
	Provider string `json:"provider,omitempty"`
	// Threads is each inference's thread count; 0 chooses from the CPU.
	Threads int `json:"threads"`
	// MaxConcurrent is how many files are transcribed at once; 0 is one.
	MaxConcurrent int `json:"max_concurrent"`
	// FFmpegPath is an ffmpeg to decode compressed audio with. Empty takes
	// WAV and raw PCM only, which is all an application that records its
	// own audio needs.
	FFmpegPath string `json:"ffmpeg_path"`
	// Mirrors are base URLs to fetch models from when the catalog's own
	// location fails, tried in order.
	Mirrors []string `json:"mirrors"`
	// HistoryTTL is how long finished jobs are kept, as a Go duration
	// ("720h"); empty is thirty days.
	HistoryTTL string `json:"history_ttl"`
	// LogLevel is debug, info, warn or error; empty is info.
	LogLevel string `json:"log_level"`
}

// Limits on what a setting may be. They are sanity bounds, not tuning.
const (
	maxThreads    = 64
	maxConcurrent = 16
)

// LoadSettings reads the settings file. A file that does not exist is not an
// error: it is an instance that has not been set up, and configured is false.
func LoadSettings(path string) (s Settings, configured bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Settings{}, false, nil
	}
	if err != nil {
		return Settings{}, false, err
	}
	if err := decodeStrict(b, &s); err != nil {
		return Settings{}, false, fmt.Errorf("%s: %w", path, err)
	}
	return s, true, nil
}

// SaveSettings replaces the settings file whole: a crash leaves the old
// settings or the new, never half of either.
func SaveSettings(path string, s Settings) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Patch applies the fields present in a JSON object to s and leaves the rest.
// A field nobody knows is refused rather than ignored: a misspelt setting
// that silently does nothing is found weeks later.
func (s Settings) Patch(body []byte) (Settings, error) {
	if err := decodeStrict(body, &s); err != nil {
		return s, core.Errorf(core.CodeInvalidRequest, "the settings are not valid: %s", err.Error())
	}
	return s, nil
}

func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON object")
	}
	return nil
}

// Lookup describes a model by id: installed or in the catalog.
type Lookup func(id string) (registry.Manifest, error)

// Validate refuses settings an instance could not start with, naming the
// field. Models are checked against the catalog rather than the disk: they
// are usually chosen before they are downloaded.
func (s Settings) Validate(lookup Lookup) error {
	model := func(param, id string, ok func(registry.Manifest) bool, want string) error {
		if id == "" {
			return nil
		}
		m, err := lookup(id)
		if err != nil {
			return core.Errorf(core.CodeInvalidRequest, "%s: %s", param, core.AsError(err).Message).WithParam(param)
		}
		if !ok(m) {
			return core.Errorf(core.CodeInvalidRequest, "%s: %s is not %s", param, id, want).WithParam(param)
		}
		return nil
	}
	if err := model("default_model", s.DefaultModel, func(m registry.Manifest) bool {
		return m.EffectiveKind() == registry.KindASR
	}, "a recognition model"); err != nil {
		return err
	}
	if err := model("realtime_model", s.RealtimeModel, func(m registry.Manifest) bool {
		return m.EffectiveKind() == registry.KindASR && m.Streaming
	}, "a streaming recognition model"); err != nil {
		return err
	}
	if err := model("diarization_model", s.DiarizationModel, func(m registry.Manifest) bool {
		return m.EffectiveKind() == registry.KindDiarization && m.Family == "sortformer"
	}, "a sortformer diarization model"); err != nil {
		return err
	}
	if s.Isolation && !s.Diarization {
		return core.Errorf(core.CodeInvalidRequest,
			"isolation needs diarization: it finds the voices with it").WithParam("isolation")
	}
	switch s.Provider {
	case "", config.ProviderCPU:
	case config.ProviderCoreML:
		if runtime.GOOS != "darwin" {
			return core.Errorf(core.CodeInvalidRequest,
				"provider coreml exists on macOS only").WithParam("provider")
		}
	default:
		return core.Errorf(core.CodeInvalidRequest,
			"provider must be cpu or coreml").WithParam("provider")
	}
	if s.Threads < 0 || s.Threads > maxThreads {
		return core.Errorf(core.CodeInvalidRequest, "threads must be 0 to %d", maxThreads).WithParam("threads")
	}
	if s.MaxConcurrent < 0 || s.MaxConcurrent > maxConcurrent {
		return core.Errorf(core.CodeInvalidRequest,
			"max_concurrent must be 0 to %d", maxConcurrent).WithParam("max_concurrent")
	}
	if s.FFmpegPath != "" {
		if st, err := os.Stat(s.FFmpegPath); err != nil || st.IsDir() {
			return core.Errorf(core.CodeInvalidRequest,
				"ffmpeg_path: no file at %s", s.FFmpegPath).WithParam("ffmpeg_path")
		}
	}
	for _, m := range s.Mirrors {
		if u, err := url.Parse(m); err != nil || u.Scheme != "https" || u.Host == "" {
			return core.Errorf(core.CodeInvalidRequest,
				"mirrors: %q is not an https URL", m).WithParam("mirrors")
		}
	}
	if s.HistoryTTL != "" {
		if d, err := time.ParseDuration(s.HistoryTTL); err != nil || d < 0 {
			return core.Errorf(core.CodeInvalidRequest,
				"history_ttl: %q is not a duration such as 720h", s.HistoryTTL).WithParam("history_ttl")
		}
	}
	switch s.LogLevel {
	case "", "debug", "info", "warn", "error":
	default:
		return core.Errorf(core.CodeInvalidRequest,
			"log_level must be debug, info, warn or error").WithParam("log_level")
	}
	return nil
}
