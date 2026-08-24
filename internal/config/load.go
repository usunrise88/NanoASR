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
