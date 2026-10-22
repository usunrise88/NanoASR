package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/config"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/diarize"
	"github.com/usunrise88/nanoasr/internal/isolation"
	"github.com/usunrise88/nanoasr/internal/ortenv"
	"github.com/usunrise88/nanoasr/internal/pool"
	"github.com/usunrise88/nanoasr/internal/registry"
	"github.com/usunrise88/nanoasr/internal/tse"
	"github.com/usunrise88/nanoasr/internal/tse/voxlock"
)

// buildIsolation sets up voice isolation without loading its models, and
// reports what they will hold in memory once loaded.
//
// Like the sortformer diarizer, the models are fetched and loaded by the
// first isolation that needs them: a server whose models are not on disk yet
// starts regardless, and an isolation that cannot get them fails with the
// command that fixes it.
func buildIsolation(
	ctx context.Context,
	cfg config.Config,
	reg *registry.Remote,
	decoder *audio.Router,
	diarizer diarize.Diarizer,
	governor *pool.Governor,
	jobs isolation.Jobs,
	log *slog.Logger,
) (*isolation.Service, *isolationModels, int, error) {
	if !cfg.Isolation.Enabled {
		return nil, nil, 0, nil
	}
	if diarizer == nil {
		// Validate refuses this; reaching it means a caller skipped it.
		return nil, nil, 0, fmt.Errorf("isolation needs diarization")
	}
	mb := 0
	for _, want := range []struct {
		id, kind string
		families []string
	}{
		{cfg.Isolation.Model, registry.KindTSE, []string{"voxlock", voxlock.FamilyCoreML}},
		{cfg.Isolation.EmbeddingModel, registry.KindEmbedding, nil},
	} {
		man, err := reg.Resolve(ctx, want.id)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("isolation model %q: %w", want.id, err)
		}
		if man.EffectiveKind() != want.kind || (want.families != nil && !slices.Contains(want.families, man.Family)) {
			return nil, nil, 0, fmt.Errorf("isolation model %q is a %s model of family %s, not %s %s",
				want.id, man.EffectiveKind(), man.Family, want.kind, strings.Join(want.families, " or "))
		}
		// The ONNX extractor needs the onnxruntime sherpa-onnx carries; the
		// CoreML builds do not.
		if man.Family == "voxlock" {
			if err := ortenv.Supported(); err != nil {
				return nil, nil, 0, fmt.Errorf("isolation: %w", err)
			}
		}
		if man.SampleRate != tse.SampleRate {
			return nil, nil, 0, fmt.Errorf("isolation model %q is %d Hz; isolation runs at %d",
				want.id, man.SampleRate, tse.SampleRate)
		}
		mb += man.Resources.ApproxRSSMB
	}

	models := &isolationModels{cfg: cfg.Isolation, reg: reg}
	ff := audio.NewFFmpegDecoder(cfg.Audio.FFmpegPath, cfg.Audio.FFmpegTimeout.Duration)
	var mp3 func(context.Context, io.ReadCloser) (io.ReadCloser, error)
	if ff != nil {
		mp3 = ff.EncodeMP3
	}
	// Before 1.5 the audio lived under temp_dir and never survived a restart;
	// whatever a previous version left there is nobody's now.
	_ = os.RemoveAll(legacyIsolationDir(cfg))
	svc, err := isolation.New(isolation.Options{
		Dir:           isolationDir(cfg),
		TTL:           cfg.Isolation.TTL.Duration,
		Concurrency:   cfg.Isolation.Concurrency,
		MaxIsolations: cfg.Isolation.MaxIsolations,
		AllowKeep:     cfg.Isolation.AllowKeep,
		MaxKept:       cfg.Isolation.MaxKept,
		LeakFloorDB:   cfg.Isolation.LeakFloorDB,
		Threads:       cfg.Isolation.NumThreads,
		Governor:      governor,
		Decode: func(ctx context.Context, r io.Reader) ([]float32, error) {
			pcm, _, err := decoder.Decode(ctx, r, audio.Options{
				TargetSampleRate: tse.SampleRate,
				ChannelMode:      core.ChannelDownmix,
				MaxDurationSec:   cfg.Audio.MaxDuration.Seconds(),
				MaxDecodedBytes:  cfg.Audio.MaxDecodedBytes,
			})
			if err != nil {
				return nil, err
			}
			return pcm[0].Samples, nil
		},
		Diarizer: diarizer,
		Engine:   models.engine,
		Jobs:     jobs,
		MP3:      mp3,
		Log:      log,
	})
	if err != nil {
		return nil, nil, 0, err
	}
	_, installed := reg.Dir(cfg.Isolation.Model)
	log.Info("voice isolation configured",
		"model", cfg.Isolation.Model,
		"embedding", cfg.Isolation.EmbeddingModel,
		"installed", installed == nil,
		"ttl", cfg.Isolation.TTL.Duration,
		"dir", isolationDir(cfg),
		"allow_keep", cfg.Isolation.AllowKeep,
		"mp3", mp3 != nil,
		"note", "loaded by the first isolation")
	// The extractor's sessions are shared; each running isolation adds its
	// own activations, which are small next to the weights.
	return svc, models, mb, nil
}

// isolationDir is where isolations keep their audio. Kept isolations must
// survive a restart and what is not kept is deleted at startup, so the
// directory must belong to this installation alone and outlive the process:
// beside the database, unless isolation.dir names another.
func isolationDir(cfg config.Config) string {
	if cfg.Isolation.Dir != "" {
		return cfg.Isolation.Dir
	}
	return filepath.Join(filepath.Dir(cfg.Storage.DBPath), "isolations")
}

// legacyIsolationDir is where versions before 1.5 kept isolations: under
// temp_dir, named by the database path so two installations sharing temp_dir
// did not delete each other's.
func legacyIsolationDir(cfg config.Config) string {
	db, err := filepath.Abs(cfg.Storage.DBPath)
	if err != nil {
		db = cfg.Storage.DBPath
	}
	h := sha256.Sum256([]byte(db))
	return filepath.Join(cfg.Storage.TempDir, "isolations-"+hex.EncodeToString(h[:4]))
}

// isolationModels loads the extractor and the embedding on first use, and
// keeps them for the life of the server.
type isolationModels struct {
	cfg config.Isolation
	reg *registry.Remote

	mu     sync.Mutex
	loaded *tse.Engine
	model  *voxlock.Model
	emb    closingEmbedder
}

// closingEmbedder is a speaker embedder the server frees at shutdown.
type closingEmbedder interface {
	tse.Embedder
	Close()
}

func (m *isolationModels) engine(ctx context.Context) (isolation.Engine, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loaded != nil {
		return m.loaded, nil
	}
	// A failure is not remembered: the operator may pull the model while
	// the server runs, and the next isolation should find it.
	path := func(id, role string) (string, error) {
		dir, err := m.reg.Ensure(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return "", err
			}
			msg := core.AsError(err).Message
			if !strings.Contains(msg, "models pull") {
				msg += "; run `nanoasr models pull " + id + "`"
			}
			return "", core.Errorf(core.CodeCapabilityUnavailable,
				"voice isolation is unavailable: %s", msg).WithCause(err)
		}
		man, err := m.reg.Resolve(ctx, id)
		if err != nil {
			return "", err
		}
		return man.FilePath(dir, role)
	}
	family := func(id string) (string, error) {
		man, err := m.reg.Resolve(ctx, id)
		if err != nil {
			return "", err
		}
		return man.Family, nil
	}
	model, err := m.loadExtractor(path, family)
	if err != nil {
		return nil, err
	}
	emb, err := m.loadEmbedder(path, family)
	if err != nil {
		_ = model.Close()
		return nil, err
	}
	m.model, m.emb = model, emb
	m.loaded = &tse.Engine{
		Model:     model,
		Embedder:  emb,
		Selection: tse.DefaultSelection(),
		Gate:      tse.DefaultGate(),
	}
	return m.loaded, nil
}

func (m *isolationModels) loadExtractor(path func(id, role string) (string, error),
	family func(string) (string, error)) (*voxlock.Model, error) {
	fam, err := family(m.cfg.Model)
	if err != nil {
		return nil, err
	}
	var files map[string]*string
	var f voxlock.Files
	var cf voxlock.CoreMLFiles
	if fam == voxlock.FamilyCoreML {
		files = map[string]*string{"enroll": &cf.Enroll, "front": &cf.Front, "back": &cf.Back}
	} else {
		files = map[string]*string{"enroll": &f.Enroll, "chunk": &f.Chunk}
	}
	for role, dst := range files {
		if *dst, err = path(m.cfg.Model, role); err != nil {
			return nil, err
		}
	}
	if fam == voxlock.FamilyCoreML {
		return voxlock.LoadCoreML(cf)
	}
	return voxlock.Load(f, ortenv.Tuning{IntraOpThreads: m.cfg.NumThreads, NoSpinning: true})
}

func (m *isolationModels) loadEmbedder(path func(id, role string) (string, error),
	family func(string) (string, error)) (closingEmbedder, error) {
	fam, err := family(m.cfg.EmbeddingModel)
	if err != nil {
		return nil, err
	}
	if fam == tse.FamilyCoreMLCampplus {
		fb, err := path(m.cfg.EmbeddingModel, "fbank")
		if err != nil {
			return nil, err
		}
		net, err := path(m.cfg.EmbeddingModel, "net")
		if err != nil {
			return nil, err
		}
		return tse.NewCoreMLEmbedder(fb, net)
	}
	p, err := path(m.cfg.EmbeddingModel, "model")
	if err != nil {
		return nil, err
	}
	return tse.NewSherpaEmbedder(p, 1)
}

// Close frees the models; the service using them must have stopped.
func (m *isolationModels) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.model != nil {
		_ = m.model.Close()
	}
	if m.emb != nil {
		m.emb.Close()
	}
	m.loaded, m.model, m.emb = nil, nil, nil
}
