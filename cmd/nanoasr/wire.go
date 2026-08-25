package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/usunrise88/nanoasr/internal/asr/sherpa"
	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/config"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/job"
	"github.com/usunrise88/nanoasr/internal/pipeline"
	"github.com/usunrise88/nanoasr/internal/pool"
	"github.com/usunrise88/nanoasr/internal/registry"
	"github.com/usunrise88/nanoasr/internal/service"
	"github.com/usunrise88/nanoasr/internal/spool"
	"github.com/usunrise88/nanoasr/internal/store/sqlite"
	"github.com/usunrise88/nanoasr/internal/vad"
	"github.com/usunrise88/nanoasr/internal/webhook"
)

// server is everything the HTTP layer needs, plus what has to be shut down.
type server struct {
	service  core.Service
	models   core.ModelService
	registry *registry.Remote
	pool     *pool.Pool
	vad      vad.Segmenter

	queue   *job.Queue
	store   *sqlite.Store
	spool   *spool.Spool
	webhook *webhook.Sender
}

// build assembles the server from configuration. Every failure here is a
// startup failure on purpose: a server that boots without a usable VAD model
// or an unreadable models directory would only fail on the first request, when
// a user is waiting.
func build(ctx context.Context, cfg config.Config, log *slog.Logger) (*server, error) {
	reg, err := buildRegistry(cfg, log)
	if err != nil {
		return nil, err
	}

	models := pool.New(reg,
		sherpa.NewLoader(sherpa.LoaderOptions{
			Provider:   "cpu",
			NumThreads: cfg.ASR.NumThreads,
			Debug:      cfg.Log.Level == "debug",
		}),
		pool.Options{
			MaxResidentModels: cfg.ASR.MaxResidentModels,
			MaxModelRSSMB:     cfg.ASR.MaxModelRSSMB,
			IdleTTL:           cfg.ASR.IdleTTL.Duration,
			AcquireTimeout:    cfg.ASR.AcquireTimeout.Duration,
			MaxVariants:       cfg.ASR.Variants.Max,
		})

	decoder := buildDecoder(cfg, log)

	segmenter, err := buildVAD(ctx, cfg, reg, log)
	if err != nil {
		models.Close()
		return nil, err
	}

	store, err := sqlite.Open(cfg.Storage.DBPath)
	if err != nil {
		models.Close()
		_ = segmenter.Close()
		return nil, err
	}

	svc := pipeline.New(decoder, segmenter, models, pool.NewGovernor(cfg.ASR.InferenceSlots),
		pipeline.Options{
			DefaultModel:     cfg.ASR.DefaultModel,
			MaxDuration:      cfg.Audio.MaxDuration.Duration,
			TargetSampleRate: cfg.Audio.TargetSampleRate,
			ChannelMode:      core.ChannelMode(cfg.Audio.ChannelMode),
			MinSilenceMS:     cfg.VAD.MinSilenceMS,
			MaxSplitChannels: cfg.Audio.MaxSplitChannels,
			MaxDecodedBytes:  cfg.Audio.MaxDecodedBytes,

			HotwordsEnabled:      cfg.PostProc.Hotwords.Enabled,
			HotwordsDefaultScore: cfg.PostProc.Hotwords.DefaultScore,
			BatchMaxSize:         cfg.ASR.Batch.MaxSize,
			BatchMaxSeconds:      cfg.ASR.Batch.MaxSeconds,
			NumThreads:           cfg.ASR.NumThreads,
		})

	hooks := webhook.New(webhook.Options{
		Secret:       cfg.Jobs.WebhookSecret,
		AllowPrivate: cfg.Jobs.WebhookAllowPrivate,
		Logger:       log,
	})

	sp := spool.New(cfg.Storage.TempDir, cfg.Jobs.MaxQueuedBytes)
	queue := job.New(store, svc, job.Options{
		Size:       cfg.Jobs.QueueSize,
		Workers:    cfg.Jobs.MaxConcurrent,
		MaxRunTime: cfg.Jobs.MaxProcessingTime.Duration,
		Hub:        job.NewHub(32),
		Spool:      sp,
		Notifier:   hooks,
		Logger:     log,
	})
	// The knot: the queue needs the pipeline as its Runner, the pipeline needs
	// the queue to submit. Tying it here keeps it visible instead of hiding it
	// behind an interface that exists only for this. Attach also routes stage
	// reports into the queue's hub, so there is no second hub to get wrong.
	svc.Attach(queue, store)

	return &server{
		service:  svc,
		models:   service.NewModels(reg, models),
		registry: reg,
		pool:     models,
		vad:      segmenter,
		queue:    queue,
		store:    store,
		spool:    sp,
		webhook:  hooks,
	}, nil
}

// resume brings the previous process's work forward.
//
// The order is load-bearing. Recover reports which jobs are live, and Sweep
// deletes every spool file that is not; running it the other way round deletes
// exactly the audio the resumed jobs are waiting for. FailStale comes after
// both, because it turns interrupted running jobs into failures whose files
// Sweep has already collected.
func (s *server) resume(ctx context.Context, log *slog.Logger) error {
	live, err := s.queue.Recover(ctx)
	if err != nil {
		return fmt.Errorf("resuming queued jobs: %w", err)
	}

	removed, err := s.spool.Sweep(live)
	if err != nil {
		return fmt.Errorf("cleaning the spool directory: %w", err)
	}
	if removed > 0 {
		log.Info("removed orphaned audio", "files", removed, "dir", s.spool.Dir())
	}

	failed, err := s.store.FailStale(ctx,
		"the server restarted while this job was running; resubmit it")
	if err != nil {
		return fmt.Errorf("failing interrupted jobs: %w", err)
	}
	if failed > 0 {
		log.Warn("jobs were interrupted by a restart", "count", failed)
	}

	s.queue.Start()
	return nil
}

// purgeHistory deletes finished jobs past their TTL, hourly.
//
// Not a cron and not a separate binary: history is the only thing that grows
// without bound here, and a goroutine that wakes up once an hour is the whole
// requirement.
func (s *server) purgeHistory(ctx context.Context, ttl time.Duration, log *slog.Logger) {
	if ttl <= 0 {
		return
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := s.store.Purge(ctx, ttl)
			if err != nil {
				log.Warn("could not purge job history", "err", err)
				continue
			}
			if n > 0 {
				log.Info("purged job history", "jobs", n, "older_than", ttl)
			}
		}
	}
}

// buildRegistry reads the models directory and layers downloading on top.
func buildRegistry(cfg config.Config, log *slog.Logger) (*registry.Remote, error) {
	local, err := registry.NewLocal(cfg.ASR.ModelsDir)
	if err != nil {
		return nil, err
	}
	for _, p := range local.Problems() {
		// A model nobody can load should be visible, not silently absent.
		log.Warn("skipping unusable model", "problem", p)
	}

	var catalogYAML []byte
	if cfg.Registry.CatalogURL != "" {
		if catalogYAML, err = os.ReadFile(cfg.Registry.CatalogURL); err != nil {
			return nil, fmt.Errorf("registry.catalog_url: %w", err)
		}
	}

	downloader := registry.NewHTTPDownloader(registry.DownloadOptions{
		Mirrors: cfg.Registry.Mirrors,
	})

	remote, err := registry.NewRemote(local, downloader, registry.RemoteOptions{
		AllowDownload: cfg.Registry.AllowDownload,
		StrictLicense: cfg.Registry.StrictLicense,
		Concurrency:   cfg.Registry.DownloadConcurrency,
		CatalogYAML:   catalogYAML,
	})
	if err != nil {
		return nil, err
	}

	if !cfg.Registry.AllowDownload {
		log.Info("model downloading is disabled", "models_dir", cfg.ASR.ModelsDir)
	}
	return remote, nil
}

// buildDecoder wires the native path first so WAV never pays for a process,
// and adds ffmpeg only when it is actually installed.
func buildDecoder(cfg config.Config, log *slog.Logger) *audio.Router {
	decoders := []audio.Decoder{audio.NewWAVDecoder()}

	if ff := audio.NewFFmpegDecoder(cfg.Audio.FFmpegPath, cfg.Audio.FFmpegTimeout.Duration); ff != nil {
		decoders = append(decoders, ff)
		log.Info("ffmpeg available", "path", ff.Path())
	} else {
		log.Warn("ffmpeg not found; only WAV and raw PCM will be accepted",
			"configured_path", cfg.Audio.FFmpegPath)
	}
	return audio.NewRouter(decoders...)
}

// buildVAD resolves the detector model through the same registry that serves
// ASR models, so there is one way to find a model file rather than two.
func buildVAD(ctx context.Context, cfg config.Config, reg *registry.Remote, log *slog.Logger) (vad.Segmenter, error) {
	if !cfg.VAD.Enabled {
		log.Warn("VAD is disabled: long recordings are decoded as one segment, " +
			"which costs accuracy and memory on exactly the files this server targets")
		return vad.Disabled{}, nil
	}

	man, err := reg.Resolve(ctx, cfg.VAD.Model)
	if err != nil {
		return nil, fmt.Errorf("vad model %q: %w", cfg.VAD.Model, err)
	}
	dir, err := reg.Dir(cfg.VAD.Model)
	if err != nil {
		return nil, err
	}
	path, err := man.FilePath(dir, "model")
	if err != nil {
		return nil, err
	}

	// One detector per concurrent job: the detector is stateful, so sharing one
	// would serialise the pipeline.
	pooled, err := vad.NewPool(vad.Config{
		Family:       man.Family,
		ModelPath:    path,
		Threshold:    cfg.VAD.Threshold,
		MinSilenceMS: cfg.VAD.MinSilenceMS,
		MinSpeechMS:  cfg.VAD.MinSpeechMS,
		MaxSpeechSec: cfg.VAD.MaxSpeechSec,
		SampleRate:   cfg.Audio.TargetSampleRate,
	}, cfg.Jobs.MaxConcurrent)
	if err != nil {
		return nil, err
	}

	log.Info("vad ready", "model", man.Key(), "instances", cfg.Jobs.MaxConcurrent)
	return pooled, nil
}

// preload warms the default model at startup so the first request does not pay
// for loading several hundred megabytes of weights.
func (s *server) preload(ctx context.Context, id string, log *slog.Logger) {
	if id == "" {
		return
	}
	if err := s.models.Load(ctx, id); err != nil {
		// Not fatal: another model may still be requested explicitly, and
		// refusing to serve at all would be a worse outcome.
		log.Warn("could not preload the default model", "model", id, "err", err)
		return
	}
	log.Info("default model loaded", "model", id)
}

// Close releases everything build acquired. The queue is stopped by the caller
// before this, while there is still a grace period to spend on it.
func (s *server) Close() {
	if s.store != nil {
		_ = s.store.Close()
	}
	if s.registry != nil {
		_ = s.registry.Close()
	}
	if s.vad != nil {
		_ = s.vad.Close()
	}
	if s.pool != nil {
		_ = s.pool.Close()
	}
}
