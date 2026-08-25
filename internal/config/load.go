package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Load builds the effective configuration: defaults, then the YAML file if the
// path is non-empty, then NANOASR_* environment overrides, then Autotune, then
// Validate. Flags are applied by the caller after Load, being highest priority.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config %s: %w", path, err)
		}
		// KnownFields makes a typo in the config a startup failure rather than
		// a silently ignored setting.
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	applyEnv(&cfg)
	cfg.Autotune()

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// applyEnv covers the settings an operator realistically overrides per
// deployment. Everything else stays in the file.
func applyEnv(c *Config) {
	str("NANOASR_ADDR", &c.Server.Addr)
	str("NANOASR_AUTH_MODE", &c.Auth.Mode)
	str("NANOASR_MODELS_DIR", &c.ASR.ModelsDir)
	str("NANOASR_DEFAULT_MODEL", &c.ASR.DefaultModel)
	str("NANOASR_FFMPEG_PATH", &c.Audio.FFmpegPath)
	str("NANOASR_DB_PATH", &c.Storage.DBPath)
	str("NANOASR_TEMP_DIR", &c.Storage.TempDir)
	str("NANOASR_WEBHOOK_SECRET", &c.Jobs.WebhookSecret)
	str("NANOASR_LOG_LEVEL", &c.Log.Level)
	str("NANOASR_LOG_FORMAT", &c.Log.Format)

	num("NANOASR_INFERENCE_SLOTS", &c.ASR.InferenceSlots)
	num("NANOASR_NUM_THREADS", &c.ASR.NumThreads)
	num("NANOASR_MAX_RESIDENT_MODELS", &c.ASR.MaxResidentModels)
	num("NANOASR_MAX_MODEL_RSS_MB", &c.ASR.MaxModelRSSMB)
	num("NANOASR_QUEUE_SIZE", &c.Jobs.QueueSize)
	num("NANOASR_MAX_CONCURRENT", &c.Jobs.MaxConcurrent)
	num64("NANOASR_MAX_QUEUED_BYTES", &c.Jobs.MaxQueuedBytes)

	bl("NANOASR_UI_ENABLED", &c.UI.Enabled)
	bl("NANOASR_ALLOW_DOWNLOAD", &c.Registry.AllowDownload)

	if v := os.Getenv("NANOASR_AUTH_KEYS"); v != "" {
		// Environment-supplied keys are unnamed and non-administrative:
		// granting admin rights should take a deliberate edit to a file, not
		// an environment variable someone copied between deployments.
		c.Auth.Keys = nil
		for _, secret := range strings.Split(v, ",") {
			if s := strings.TrimSpace(secret); s != "" {
				c.Auth.Keys = append(c.Auth.Keys, APIKey{Key: s})
			}
		}
	}
	if v := os.Getenv("NANOASR_API_DIALECTS"); v != "" {
		c.API.Dialects = strings.Split(v, ",")
	}
	if v := os.Getenv("NANOASR_MAX_DURATION"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Audio.MaxDuration = Dur(d)
		}
	}
}

func str(key string, dst *string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func num(key string, dst *int) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

func num64(key string, dst *int64) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			*dst = n
		}
	}
}

func bl(key string, dst *bool) {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			*dst = b
		}
	}
}

// Validate rejects configurations that would fail confusingly at request time.
func (c *Config) Validate() error {
	switch c.Auth.Mode {
	case "apikey":
		// A server in apikey mode with no keys answers 401 to everything: it
		// is running, it looks configured, and it is useless. Say so at
		// startup rather than at the first request.
		if len(c.Auth.Keys) == 0 {
			return fmt.Errorf("auth.mode=apikey but no keys are configured; " +
				"set auth.keys or NANOASR_AUTH_KEYS, or use auth.mode=open on a loopback address")
		}
		for i, k := range c.Auth.Keys {
			if strings.TrimSpace(k.Key) == "" {
				return fmt.Errorf("auth.keys[%d] (%q) has no key value", i, k.Name)
			}
			switch k.Priority {
			case "", PriorityBatch, PriorityInteractive:
			default:
				return fmt.Errorf(
					"auth.keys[%d] (%q): priority must be %q or %q, got %q",
					i, k.Name, PriorityInteractive, PriorityBatch, k.Priority)
			}
			if k.RPS < 0 {
				return fmt.Errorf("auth.keys[%d] (%q): rps must not be negative", i, k.Name)
			}
		}
	case "open":
		// An unauthenticated listener on a routable address is not a
		// configuration mistake we are willing to boot with.
		if !isLoopback(c.Server.Addr) {
			return fmt.Errorf("auth.mode=open requires a loopback listen address, got %q", c.Server.Addr)
		}
	default:
		return fmt.Errorf("auth.mode must be apikey or open, got %q", c.Auth.Mode)
	}

	switch c.Audio.ChannelMode {
	case "downmix", "first", "split":
	default:
		return fmt.Errorf("audio.channel_mode must be downmix, first or split, got %q", c.Audio.ChannelMode)
	}

	if c.Audio.TargetSampleRate != 16000 {
		// Every bundled VAD and diarization model is 16 kHz; letting this
		// drift produces silently wrong timings rather than an error.
		return fmt.Errorf("audio.target_sample_rate must be 16000, got %d", c.Audio.TargetSampleRate)
	}
	if c.Server.MaxUploadBytes <= 0 {
		return fmt.Errorf("server.max_upload_bytes must be positive")
	}
	if c.Jobs.QueueSize <= 0 {
		return fmt.Errorf("jobs.queue_size must be positive")
	}
	// A budget below one upload rejects every submission the moment the queue
	// has anything in it, which reads as an intermittent 429 rather than as a
	// misconfiguration.
	if c.Jobs.MaxQueuedBytes > 0 && c.Jobs.MaxQueuedBytes < c.Server.MaxUploadBytes {
		return fmt.Errorf(
			"jobs.max_queued_bytes (%d) is below server.max_upload_bytes (%d): "+
				"no upload at the size limit could ever be queued",
			c.Jobs.MaxQueuedBytes, c.Server.MaxUploadBytes)
	}
	if c.Storage.DBPath == "" {
		return fmt.Errorf("storage.db_path must be set: the job queue needs somewhere to record work")
	}
	// The webhook address check is what keeps webhook_url from reaching into
	// the network the server sits in, so turning it off carries the same
	// condition as turning authentication off.
	if c.Jobs.WebhookAllowPrivate && !isLoopback(c.Server.Addr) {
		return fmt.Errorf(
			"jobs.webhook_allow_private requires a loopback listen address, got %q", c.Server.Addr)
	}
	if len(c.API.Dialects) == 0 && !c.UI.Enabled {
		return fmt.Errorf("no api dialects enabled and ui disabled: server would serve nothing")
	}
	if c.Audio.MaxSplitChannels < 1 {
		return fmt.Errorf("audio.max_split_channels must be at least 1, got %d", c.Audio.MaxSplitChannels)
	}
	if err := c.validatePostProc(); err != nil {
		return err
	}
	return c.validateDiarization()
}

// validatePostProc refuses post-processing that is switched on but has nothing
// to run with. The alternative is a server that starts, looks configured, and
// answers every punctuate=true with a warning nobody expected.
func (c *Config) validatePostProc() error {
	if c.PostProc.Punctuation.Enabled && c.PostProc.Punctuation.Model == "" {
		return fmt.Errorf("postproc.punctuation.enabled is true but postproc.punctuation.model is empty; " +
			"name a model of kind punctuation, or leave punctuation disabled")
	}
	if c.PostProc.ITN.Enabled && c.PostProc.ITN.Locale == "" {
		return fmt.Errorf("postproc.itn.enabled is true but postproc.itn.locale is empty")
	}
	if c.PostProc.Hotwords.DefaultScore < 0 {
		return fmt.Errorf("postproc.hotwords.default_score must not be negative, got %v",
			c.PostProc.Hotwords.DefaultScore)
	}
	if c.ASR.Variants.Max < 0 {
		return fmt.Errorf("asr.variants.max must not be negative, got %d", c.ASR.Variants.Max)
	}
	// Allowing hotwords without a variant budget is the configuration that
	// looks enabled and does nothing: every request would be answered with the
	// model's own decoding and a warning.
	if c.PostProc.Hotwords.Enabled && c.ASR.Variants.Max == 0 {
		return fmt.Errorf("postproc.hotwords.enabled is true but asr.variants.max is 0; " +
			"per-request hotwords need a second resident model instance to load into")
	}
	return nil
}

func (c *Config) validateDiarization() error {
	if !c.Diarization.Enabled {
		return nil
	}
	if c.Diarization.SegmentationModel == "" || c.Diarization.EmbeddingModel == "" {
		return fmt.Errorf("diarization.enabled is true but segmentation_model or embedding_model is empty; " +
			"diarization needs both a segmentation and an embedding model")
	}
	if c.Diarization.Clustering.NumClusters < 0 {
		return fmt.Errorf("diarization.clustering.num_clusters must not be negative, got %d",
			c.Diarization.Clustering.NumClusters)
	}
	// Threshold is only consulted when the speaker count is unknown, so an
	// out-of-range value is harmless until the day it is not.
	if c.Diarization.Clustering.NumClusters == 0 {
		if t := c.Diarization.Clustering.Threshold; t <= 0 || t >= 1 {
			return fmt.Errorf("diarization.clustering.threshold must be between 0 and 1 exclusive, got %v; "+
				"it is what decides speaker count when num_clusters is 0", t)
		}
	}
	if c.Diarization.MinDurationOn < 0 || c.Diarization.MinDurationOff < 0 {
		return fmt.Errorf("diarization.min_duration_on and min_duration_off must not be negative")
	}
	return nil
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
