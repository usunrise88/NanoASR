package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/usunrise88/nanoasr/internal/asr/sherpa"
	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/config"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/pipeline"
	"github.com/usunrise88/nanoasr/internal/pool"
	"github.com/usunrise88/nanoasr/internal/registry"
	"github.com/usunrise88/nanoasr/internal/service"
	"github.com/usunrise88/nanoasr/internal/vad"
)

// server is everything the HTTP layer needs, plus what has to be shut down.
type server struct {
	service  core.Service
	models   core.ModelService
	registry *registry.Remote
	pool     *pool.Pool
	vad      vad.Segmenter
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
		})

	decoder := buildDecoder(cfg, log)

	segmenter, err := buildVAD(ctx, cfg, reg, log)
	if err != nil {
		models.Close()
		return nil, err
	}

	svc := pipeline.New(decoder, segmenter, models, pool.NewGovernor(cfg.ASR.InferenceSlots),
		pipeline.Options{
			DefaultModel:     cfg.ASR.DefaultModel,
			MaxDuration:      cfg.Audio.MaxDuration.Duration,
			TargetSampleRate: cfg.Audio.TargetSampleRate,
			ChannelMode:      core.ChannelMode(cfg.Audio.ChannelMode),
			MinSilenceMS:     cfg.VAD.MinSilenceMS,
			BatchMaxSize:     cfg.ASR.Batch.MaxSize,
			BatchMaxSeconds:  cfg.ASR.Batch.MaxSeconds,
			NumThreads:       cfg.ASR.NumThreads,
		})

	return &server{
		service:  svc,
		models:   service.NewModels(reg, models),
		registry: reg,
		pool:     models,
		vad:      segmenter,
	}, nil
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

func (s *server) Close() {
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
