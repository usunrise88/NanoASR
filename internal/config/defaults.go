package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Default returns a configuration that starts a working server on any machine.
// Sizing knobs left at zero are derived from the host in Autotune.
func Default() Config {
	return Config{
		Server: Server{
			Addr:              ":8080",
			ReadHeaderTimeout: Dur(10 * time.Second),
			MaxUploadBytes:    100 << 20,
			ShutdownGrace:     Dur(30 * time.Second),
		},
		Auth: Auth{Mode: "apikey"},
		API:  API{Dialects: []string{"openai", "native"}},
		UI:   UI{Enabled: true, Path: "/ui"},
		Audio: Audio{
			FFmpegPath:       "ffmpeg",
			FFmpegTimeout:    Dur(120 * time.Second),
			MaxDuration:      Dur(30 * time.Minute),
			TargetSampleRate: 16000,
			ChannelMode:      "downmix",
		},
		VAD: VAD{
			Enabled:      true,
			Model:        "silero-vad-v5",
			Threshold:    0.5,
			MinSilenceMS: 300,
			MinSpeechMS:  250,
			MaxSpeechSec: 20,
		},
		ASR: ASR{
			ModelsDir:      "/var/lib/nanoasr/models",
			IdleTTL:        Dur(15 * time.Minute),
			AcquireTimeout: Dur(30 * time.Second),
			Batch:          Batch{MaxSize: 8, MaxSeconds: 60},
		},
		Registry: Registry{AllowDownload: true, DownloadConcurrency: 2},
		Jobs: Jobs{
			QueueSize: 100,
			// Four gigabytes: enough for a realistic backlog of hour-long
			// recordings, small enough to fit beside a database and a model
			// cache on the modest disk this server usually gets.
			MaxQueuedBytes:    4 << 30,
			MaxProcessingTime: Dur(30 * time.Minute),
			HistoryTTL:        Dur(30 * 24 * time.Hour),
		},
		PostProc:    PostProc{ITN: ITN{Locale: "ru"}},
		Diarization: Diarization{Clustering: Clustering{Threshold: 0.5}},
		Storage:     Storage{DBPath: "/var/lib/nanoasr/nanoasr.db"},
		Log:         Log{Level: "info", Format: "json"},
	}
}

// Autotune fills sizing knobs that are still zero, using CPU count and total
// RAM. Every formula here is documented in SPEC §12.3; keep them in sync.
func (c *Config) Autotune() {
	cpus := runtime.GOMAXPROCS(0)
	ramMB := totalRAMMB()

	if c.ASR.InferenceSlots <= 0 {
		c.ASR.InferenceSlots = cpus
	}
	if c.ASR.NumThreads <= 0 {
		c.ASR.NumThreads = clamp(cpus/2, 1, 8)
	}
	if c.Jobs.MaxConcurrent <= 0 {
		c.Jobs.MaxConcurrent = clamp(cpus/c.ASR.NumThreads, 1, 8)
	}
	if c.ASR.MaxModelRSSMB <= 0 {
		c.ASR.MaxModelRSSMB = clamp(ramMB/2, 1024, 16384)
	}
	if c.ASR.MaxResidentModels <= 0 {
		c.ASR.MaxResidentModels = clamp(c.ASR.MaxModelRSSMB/1500, 1, 6)
	}
	// An unset temp_dir means "choose one", and it is a documented value in the
	// shipped configuration files — so it is resolved here rather than left as
	// the empty path for something further down to fail on.
	//
	// A subdirectory of our own, not the system temp directory itself: the
	// spool holds uploaded audio at 0700, and startup cleanup should be walking
	// our files rather than everyone else's.
	if c.Storage.TempDir == "" {
		c.Storage.TempDir = filepath.Join(os.TempDir(), "nanoasr-spool")
	}
}

// PeakMemoryEstimateMB is what the process is expected to need at full load:
// resident models plus the decoded PCM of every concurrent job. 30 minutes of
// mono float32 at 16 kHz is ~115 MB, which dominates on long files.
func (c *Config) PeakMemoryEstimateMB() int {
	pcmPerJob := int(c.Audio.MaxDuration.Seconds()) * c.Audio.TargetSampleRate * 4 / (1 << 20)
	return c.ASR.MaxModelRSSMB + c.Jobs.MaxConcurrent*pcmPerJob
}

func totalRAMMB() int {
	// MemTotal is the only field we need and it is always the first line.
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 4096
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			break
		}
		kb, err := strconv.Atoi(f[1])
		if err != nil {
			break
		}
		return kb / 1024
	}
	return 4096
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
