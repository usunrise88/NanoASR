package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/usunrise88/nanoasr/internal/asr/sherpa"
	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/config"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/diarize"
	diarizesherpa "github.com/usunrise88/nanoasr/internal/diarize/sherpa"
	diarizesortformer "github.com/usunrise88/nanoasr/internal/diarize/sortformer"
	"github.com/usunrise88/nanoasr/internal/job"
	"github.com/usunrise88/nanoasr/internal/pipeline"
	"github.com/usunrise88/nanoasr/internal/pool"
	"github.com/usunrise88/nanoasr/internal/postproc"
	postprocsherpa "github.com/usunrise88/nanoasr/internal/postproc/sherpa"
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
	diarizer diarize.Diarizer
	// diarizerMB is what the diarizer is expected to hold at full load, for
	// the startup memory estimate.
	diarizerMB int
	closePost  func()

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

	diarizer, diarizerMB, err := buildDiarizer(ctx, cfg, reg, log)
	if err != nil {
		models.Close()
		_ = segmenter.Close()
		return nil, err
	}

	post, closePost, err := buildPostProc(ctx, cfg, reg, log)
	if err != nil {
		models.Close()
		_ = segmenter.Close()
		closeDiarizer(diarizer)
		return nil, err
	}

	store, err := sqlite.Open(cfg.Storage.DBPath)
	if err != nil {
		models.Close()
		_ = segmenter.Close()
		closeDiarizer(diarizer)
		closePost()
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
		}).WithDiarizer(diarizer).WithPostProc(post)

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
		service:    svc,
		models:     service.NewModels(reg, models),
		registry:   reg,
		pool:       models,
		vad:        segmenter,
		diarizer:   diarizer,
		diarizerMB: diarizerMB,
		closePost:  closePost,
		queue:      queue,
		store:      store,
		spool:      sp,
		webhook:    hooks,
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

// purgePool enforces asr.idle_ttl. The configured TTL is otherwise dead
// configuration: Sweep exists but nothing was calling it, and a model loaded
// once stayed resident forever (a configured pool ceiling without a clock is
// a memory ceiling on paper only).
func (s *server) purgePool(ctx context.Context, idleTTL time.Duration, log *slog.Logger) {
	if idleTTL <= 0 {
		return
	}
	period := idleTTL / 2
	if period > time.Minute {
		period = time.Minute
	}
	ticker := time.NewTicker(period)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			if n := s.pool.Sweep(t); n > 0 {
				log.Info("evicted idle models", "n", n, "idle_ttl", idleTTL)
			}
		}
	}
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

// preloadDiarizer loads a lazily loaded diarizer in the background, so the
// first diarized request does not pay for it. Failing is not fatal: the first
// request tries again and, if it cannot either, says why in a warning.
func (s *server) preloadDiarizer(ctx context.Context, log *slog.Logger) {
	p, ok := s.diarizer.(interface{ Preload(context.Context) error })
	if !ok {
		return
	}
	start := time.Now()
	if err := p.Preload(ctx); err != nil {
		if ctx.Err() == nil {
			log.Warn("could not preload the diarization model; the first diarized request will try again",
				"err", err)
		}
		return
	}
	log.Info("diarization model loaded", "ms", time.Since(start).Milliseconds())
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
	closeDiarizer(s.diarizer)
	if s.closePost != nil {
		s.closePost()
	}
	if s.pool != nil {
		_ = s.pool.Close()
	}
}

// buildDiarizer builds the configured diarization backend and reports what it
// is expected to hold in memory at full load.
//
// Like the VAD pool, this sits outside the model pool: these are not
// recognisers, they are not selectable per request, and once loaded they stay
// loaded for the life of the server. That means they do not participate in LRU
// eviction or max_model_rss_mb, which is a deliberate trade — the alternative is
// a pool key space where "model" means two unrelated things. Their memory is
// counted in the startup estimate instead.
func buildDiarizer(
	ctx context.Context,
	cfg config.Config,
	reg *registry.Remote,
	log *slog.Logger,
) (diarize.Diarizer, int, error) {
	if !cfg.Diarization.Enabled {
		return nil, 0, nil
	}
	if cfg.Diarization.Backend == config.DiarizationSherpa {
		return buildSherpaDiarizer(ctx, cfg, reg, log)
	}
	return buildSortformer(ctx, cfg, reg, log)
}

// buildSortformer sets up Nemotron-3-Diarization without loading it.
//
// The model is fetched and loaded by the first request that asks for
// diarization, as ASR models are: a server upgraded to this default, or one
// whose model is not on disk yet, starts regardless, and one that cannot
// download it answers diarized requests with a diarization_unavailable warning
// naming the command that fixes it.
func buildSortformer(
	ctx context.Context,
	cfg config.Config,
	reg *registry.Remote,
	log *slog.Logger,
) (diarize.Diarizer, int, error) {
	id := cfg.Diarization.Model
	man, err := reg.Resolve(ctx, id)
	if err != nil {
		return nil, 0, fmt.Errorf("diarization.model %q: %w", id, err)
	}
	if man.EffectiveKind() != registry.KindDiarization || man.Family != "sortformer" {
		return nil, 0, fmt.Errorf("diarization.model %q is a %s model of family %s, not a sortformer diarizer",
			id, man.EffectiveKind(), man.Family)
	}
	// Caught at startup for the reason given in buildSherpaDiarizer.
	if man.SampleRate != cfg.Audio.TargetSampleRate {
		return nil, 0, fmt.Errorf(
			"diarization model %s expects %d Hz but audio.target_sample_rate is %d; "+
				"set them to the same rate", id, man.SampleRate, cfg.Audio.TargetSampleRate)
	}

	d, err := diarizesortformer.New(diarizesortformer.Options{
		NumThreads: cfg.ASR.NumThreads,
		Resolve: func(ctx context.Context) (diarizesortformer.Files, error) {
			dir, err := reg.Ensure(ctx, id)
			if err != nil {
				if ctx.Err() != nil {
					return diarizesortformer.Files{}, err
				}
				// the registry's own reason usually names the fix already
				msg := core.AsError(err).Message
				if !strings.Contains(msg, "models pull") {
					msg += "; run `nanoasr models pull " + id + "`"
				}
				return diarizesortformer.Files{}, core.Errorf(core.CodeCapabilityUnavailable,
					"diarization is unavailable: %s", msg).WithCause(err)
			}
			// the installed manifest, which is the one that names its files
			man, err := reg.Resolve(ctx, id)
			if err != nil {
				return diarizesortformer.Files{}, err
			}
			var f diarizesortformer.Files
			for _, r := range []struct {
				role string
				dst  *string
			}{
				{"embed", &f.Embed}, {"step", &f.Step}, {"mel_filters", &f.MelFilters},
				{"silence_embeds", &f.Silence}, {"config", &f.Config},
			} {
				if *r.dst, err = man.FilePath(dir, r.role); err != nil {
					return diarizesortformer.Files{}, err
				}
			}
			return f, nil
		},
	})
	if err != nil {
		return nil, 0, err
	}

	// One shared session: the weights once, plus the activations of every
	// step that can be in flight at the same time.
	mb := man.Resources.ApproxRSSMB + (cfg.Jobs.MaxConcurrent-1)*diarizesortformer.StepActivationsMB

	def := config.Default().Diarization
	if cfg.Diarization.Clustering != def.Clustering ||
		cfg.Diarization.MinDurationOn != def.MinDurationOn || cfg.Diarization.MinDurationOff != def.MinDurationOff {
		log.Warn("diarization.clustering, min_duration_on and min_duration_off apply to backend: sherpa "+
			"and are ignored by sortformer; set diarization.backend: sherpa to keep using them",
			"backend", config.DiarizationSortformer)
	}
	_, installed := reg.Dir(id)
	log.Info("diarization configured",
		"backend", config.DiarizationSortformer,
		"model", id,
		"installed", installed == nil,
		"note", "loaded by the first request that asks for speakers")
	return d, mb, nil
}

// buildSherpaDiarizer resolves the two speaker models through the same
// registry that serves ASR models, and builds one instance per concurrent job.
func buildSherpaDiarizer(
	ctx context.Context,
	cfg config.Config,
	reg *registry.Remote,
	log *slog.Logger,
) (diarize.Diarizer, int, error) {
	mb := 0
	resolve := func(id string) (string, error) {
		man, err := reg.Resolve(ctx, id)
		if err != nil {
			return "", fmt.Errorf("diarization model %q: %w", id, err)
		}
		dir, err := reg.Dir(id)
		if err != nil {
			return "", err
		}
		mb += man.Resources.ApproxRSSMB
		return man.FilePath(dir, "model")
	}

	segmentation, err := resolve(cfg.Diarization.SegmentationModel)
	if err != nil {
		return nil, 0, err
	}
	embedding, err := resolve(cfg.Diarization.EmbeddingModel)
	if err != nil {
		return nil, 0, err
	}

	pooled, err := diarizesherpa.NewPool(diarize.Config{
		SegmentationModel: segmentation,
		EmbeddingModel:    embedding,
		NumClusters:       cfg.Diarization.Clustering.NumClusters,
		Threshold:         cfg.Diarization.Clustering.Threshold,
		MinDurationOn:     cfg.Diarization.MinDurationOn,
		MinDurationOff:    cfg.Diarization.MinDurationOff,
	}, cfg.ASR.NumThreads, cfg.Jobs.MaxConcurrent)
	if err != nil {
		return nil, 0, err
	}

	// The pipeline hands the diarizer whatever audio.target_sample_rate
	// produced, and sherpa-onnx does not check: feeding 8 kHz to a 16 kHz
	// model is not an error there, it is a recording played at double speed,
	// with turn boundaries in the wrong place and embeddings that cluster
	// everything into one speaker. Caught here, at startup, where it is a
	// configuration mistake with an obvious fix.
	if want := pooled.SampleRate(); want > 0 && want != cfg.Audio.TargetSampleRate {
		_ = pooled.Close()
		return nil, 0, fmt.Errorf(
			"diarization models expect %d Hz but audio.target_sample_rate is %d; "+
				"set them to the same rate", want, cfg.Audio.TargetSampleRate)
	}

	log.Info("diarization ready",
		"backend", config.DiarizationSherpa,
		"segmentation", cfg.Diarization.SegmentationModel,
		"embedding", cfg.Diarization.EmbeddingModel,
		"instances", cfg.Jobs.MaxConcurrent,
		"note", "a sherpa diarization pass cannot be cancelled once started (SPEC §2 decision 34)")
	// every instance holds its own copy of both models
	return pooled, mb * cfg.Jobs.MaxConcurrent, nil
}

// closeDiarizer is nil-safe: diarization is off by default, so most servers
// never build one.
func closeDiarizer(d diarize.Diarizer) {
	if d != nil {
		_ = d.Close()
	}
}

// buildPostProc assembles the optional text stages.
//
// The punctuation model is resolved through the registry like every other
// model, and its pool is sized to the job concurrency for the same reason the
// VAD pool is: an instance is stateful, and sharing one would serialise the
// stage behind whichever job got there first.
func buildPostProc(
	ctx context.Context,
	cfg config.Config,
	reg *registry.Remote,
	log *slog.Logger,
) (*postproc.Factory, func(), error) {
	f := &postproc.Factory{
		ITNLocale:          cfg.PostProc.ITN.Locale,
		PunctuationDefault: cfg.PostProc.Punctuation.Enabled,
		ITNDefault:         cfg.PostProc.ITN.Enabled,
	}
	if cfg.PostProc.Punctuation.Model == "" {
		return f, func() {}, nil
	}

	man, err := reg.Resolve(ctx, cfg.PostProc.Punctuation.Model)
	if err != nil {
		return nil, nil, fmt.Errorf("punctuation model %q: %w", cfg.PostProc.Punctuation.Model, err)
	}
	dir, err := reg.Dir(cfg.PostProc.Punctuation.Model)
	if err != nil {
		return nil, nil, err
	}
	path, err := man.FilePath(dir, "model")
	if err != nil {
		return nil, nil, err
	}

	pooled, err := postprocsherpa.NewPool(path, cfg.ASR.NumThreads, cfg.Jobs.MaxConcurrent)
	if err != nil {
		return nil, nil, err
	}
	f.Punct = pooled

	log.Info("punctuation ready",
		"model", cfg.PostProc.Punctuation.Model,
		"instances", cfg.Jobs.MaxConcurrent,
		"note", "used only for models that do not punctuate themselves")
	return f, func() { _ = pooled.Close() }, nil
}
