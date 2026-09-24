package sortformer

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"sync"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/diarize"
)

// Files are the model's files on disk.
type Files struct {
	Embed      string // embed.onnx: log-mel -> encoder input embeddings
	Step       string // step.onnx: embeddings -> speaker logits
	MelFilters string // mel_filters.bin: float32 [128][257]
	Silence    string // silence_embeds.bin: float32 [512]
	Config     string // config.json: the offline pass's parameters
}

// Options configures a Diarizer.
type Options struct {
	// Resolve finds the model, downloading it if it has to. It is called by the
	// first Process, not at construction, so that a server whose model is not
	// on disk yet still starts, and fetches it only when someone asks for
	// diarization — the way the ASR pool treats its models.
	Resolve func(ctx context.Context) (Files, error)
	// NumThreads is the thread budget of one step, the slots the pipeline
	// reserves from its governor around the diarize stage.
	NumThreads int
}

// Diarizer is Nemotron-3-Diarization: an end-to-end Sortformer that labels up
// to eight speakers directly, in order of first arrival, with no clustering and
// so no threshold to tune.
//
// One instance serves every concurrent job: the onnxruntime sessions are shared
// (see ortRunner) and everything a recording needs — the speaker cache, the
// buffers — lives inside its Process call.
type Diarizer struct {
	opt Options

	// life is held for reading by every Process and for writing by Close, so
	// Close waits for the passes in flight instead of freeing their sessions.
	life   sync.RWMutex
	closed bool

	loadMu sync.Mutex
	m      *model
}

// model is the loaded network and what the chunk loop needs to drive it.
type model struct {
	r      runner
	fe     *frontend
	params loopParams
}

// New returns a Diarizer that loads its model on first use.
func New(opt Options) (*Diarizer, error) {
	if opt.Resolve == nil {
		return nil, core.Errorf(core.CodeInternal, "sortformer: no model resolver")
	}
	if _, err := runtimeLibrary(); err != nil {
		return nil, err
	}
	return &Diarizer{opt: opt}, nil
}

// SampleRate is the rate the model was trained at. The pipeline must resample
// to it before diarization; the front end does not.
func (d *Diarizer) SampleRate() int { return sampleRate }

// Process labels who speaks when. numClusters is ignored: Sortformer decides
// the number of speakers itself and has no way to be told.
func (d *Diarizer) Process(ctx context.Context, pcm audio.PCM, _ int) ([]diarize.Turn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.life.RLock()
	defer d.life.RUnlock()
	if d.closed {
		return nil, core.Errorf(core.CodeInternal, "diarizer is closed")
	}
	if pcm.SampleRate != sampleRate {
		return nil, core.Errorf(core.CodeInternal,
			"sortformer diarization needs %d Hz audio, got %d", sampleRate, pcm.SampleRate)
	}
	if numFrames(len(pcm.Samples)) == 0 {
		return nil, nil
	}

	m, err := d.load(ctx)
	if err != nil {
		return nil, err
	}
	tr := newTurnTracker(numFrames(len(pcm.Samples)))
	if err := m.run(ctx, pcm.Samples, tr.add); err != nil {
		return nil, err
	}
	return tr.turns(pcm.Duration()), nil
}

// run is the offline pass over one recording's samples.
func (m *model) run(ctx context.Context, x []float32, sink logitSink) error {
	melFrames := numFrames(len(x))
	frames := (melFrames + subsampling - 1) / subsampling
	feats := make([]float32, (m.params.chunkLen+m.params.rightCtx)*subsampling*melBins)
	src := func(from, to int, dst []float32) error {
		lo, hi := from*subsampling, min(to*subsampling, melFrames)
		buf := feats[:(to-from)*subsampling*melBins]
		m.fe.features(x, lo, hi, buf[:(hi-lo)*melBins])
		// the reference zero-pads the last group of eight
		clear(buf[(hi-lo)*melBins:])
		e, err := m.r.embed(buf, (to-from)*subsampling)
		if err != nil {
			return err
		}
		copy(dst, e)
		return nil
	}
	return runOffline(ctx, m.r, m.params, frames, src, sink)
}

// load builds the model the first time it is needed. A failure is not
// remembered: the next request tries again, so a download that failed once, or
// a model pulled after the server started, does not need a restart.
func (d *Diarizer) load(ctx context.Context) (*model, error) {
	d.loadMu.Lock()
	defer d.loadMu.Unlock()
	if d.m != nil {
		return d.m, nil
	}
	files, err := d.opt.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	m, err := loadModel(files, d.opt.NumThreads)
	if err != nil {
		return nil, err
	}
	d.m = m
	return m, nil
}

func loadModel(f Files, threads int) (*model, error) {
	params, err := readConfig(f.Config)
	if err != nil {
		return nil, err
	}
	filters, err := readFloats(f.MelFilters, melBins*freqBins)
	if err != nil {
		return nil, err
	}
	fe, err := newFrontend(filters)
	if err != nil {
		return nil, err
	}
	params.silence, err = readFloats(f.Silence, hiddenSize)
	if err != nil {
		return nil, err
	}
	r, err := newORTRunner(f.Embed, f.Step, sessionTuning{IntraOpThreads: threads})
	if err != nil {
		return nil, err
	}
	return &model{r: r, fe: fe, params: params}, nil
}

// modelConfig is config.json, the parameters of the reference's offline pass
// as scripts/diar-research/golden_sortformer.py reads them off the model.
type modelConfig struct {
	SampleRate            int     `json:"sample_rate"`
	NFFT                  int     `json:"n_fft"`
	HopLength             int     `json:"hop_length"`
	WinLength             int     `json:"win_length"`
	NumMelBins            int     `json:"num_mel_bins"`
	Preemphasis           float64 `json:"preemphasis"`
	LogZeroGuard          float64 `json:"log_zero_guard"`
	SubsamplingFactor     int     `json:"subsampling_factor"`
	HiddenSize            int     `json:"hidden_size"`
	MaxPositionEmbeddings int     `json:"max_position_embeddings"`
	NumSpeakers           int     `json:"num_speakers"`
	ChunkLength           int     `json:"chunk_length"`
	ChunkRightContext     int     `json:"chunk_right_context"`
	FIFOLength            int     `json:"fifo_length"`
	UpdatePeriod          int     `json:"speaker_cache_update_period"`
	CacheLength           int     `json:"speaker_cache_length"`
	SilenceSlots          int     `json:"speaker_cache_silence_frames_per_speaker"`
	ScoreThreshold        float64 `json:"prediction_score_threshold"`
	LatestBoost           float64 `json:"latest_frames_score_boost"`
	MinPositiveRate       float64 `json:"min_positive_scores_rate"`
	StrongRate            float64 `json:"strong_boost_rate"`
	WeakRate              float64 `json:"weak_boost_rate"`
	SpeechThreshold       float64 `json:"speech_threshold"`
}

func readConfig(path string) (loopParams, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return loopParams{}, core.Errorf(core.CodeInternal, "sortformer config").WithCause(err)
	}
	var c modelConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return loopParams{}, core.Errorf(core.CodeInternal, "sortformer config %s", path).WithCause(err)
	}
	// What the front end, the buffers and the thresholding are compiled for.
	fixed := []struct {
		name      string
		got, want float64
	}{
		{"sample_rate", float64(c.SampleRate), sampleRate},
		{"n_fft", float64(c.NFFT), nFFT},
		{"hop_length", float64(c.HopLength), hopLength},
		{"win_length", float64(c.WinLength), winLength},
		{"num_mel_bins", float64(c.NumMelBins), melBins},
		{"preemphasis", c.Preemphasis, preemphasis},
		{"log_zero_guard", c.LogZeroGuard, logZeroGuard},
		{"subsampling_factor", float64(c.SubsamplingFactor), subsampling},
		{"hidden_size", float64(c.HiddenSize), hiddenSize},
		{"num_speakers", float64(c.NumSpeakers), numSpeakers},
		{"speech_threshold", c.SpeechThreshold, 0.5},
	}
	for _, f := range fixed {
		if f.got != f.want {
			return loopParams{}, core.Errorf(core.CodeInternal,
				"sortformer config %s: %s is %g, this build supports only %g", path, f.name, f.got, f.want)
		}
	}
	if c.ChunkLength < 1 || c.ChunkRightContext < 0 || c.ChunkRightContext > c.ChunkLength ||
		c.FIFOLength < 0 || c.UpdatePeriod < 1 || c.CacheLength < numSpeakers*(c.SilenceSlots+1) {
		return loopParams{}, core.Errorf(core.CodeInternal, "sortformer config %s: inconsistent cache sizes", path)
	}
	p := loopParams{
		chunkLen: c.ChunkLength, rightCtx: c.ChunkRightContext,
		cache: newCacheParams(c.CacheLength, c.SilenceSlots, c.FIFOLength, c.UpdatePeriod,
			c.ScoreThreshold, c.LatestBoost, c.MinPositiveRate, c.StrongRate, c.WeakRate),
	}.withMax()
	if p.maxFrames > c.MaxPositionEmbeddings {
		return loopParams{}, core.Errorf(core.CodeInternal,
			"sortformer config %s: a step of %d frames exceeds the model's %d positions",
			path, p.maxFrames, c.MaxPositionEmbeddings)
	}
	return p, nil
}

func readFloats(path string, n int) ([]float32, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "sortformer model file").WithCause(err)
	}
	if len(b) != 4*n {
		return nil, core.Errorf(core.CodeInternal, "%s is %d bytes, want %d float32", path, len(b), n)
	}
	out := make([]float32, n)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out, nil
}

func (d *Diarizer) Close() error {
	d.life.Lock()
	defer d.life.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	d.loadMu.Lock()
	defer d.loadMu.Unlock()
	if d.m == nil {
		return nil
	}
	err := d.m.r.Close()
	d.m = nil
	return err
}

var _ diarize.Diarizer = (*Diarizer)(nil)

// turnTracker turns 10 ms logits into speaker turns as they arrive, the way
// the reference's extract_speaker_dict does: a speaker is active in a frame
// whose probability is above 0.5 — whose logit is above 0 — and a turn is a run
// of such frames. Turns of different speakers may overlap.
type turnTracker struct {
	valid int              // frames past this are the recording's padding
	next  int              // the next 10 ms frame expected
	open  [numSpeakers]int // start frame of each speaker's open turn, -1 if none
	done  []diarize.Turn   // closed turns, in frames for now
}

func newTurnTracker(validFrames int) *turnTracker {
	t := &turnTracker{valid: validFrames}
	for s := range t.open {
		t.open[s] = -1
	}
	return t
}

// add takes the logits of encoder frames starting at first, in order.
func (t *turnTracker) add(first int, rows []float32) {
	start := first * subsampling
	if start != t.next {
		panic(fmt.Sprintf("turnTracker: frame %d after %d", start, t.next))
	}
	n := len(rows) / numSpeakers
	for i := 0; i < n && start+i < t.valid; i++ {
		f := start + i
		for s := 0; s < numSpeakers; s++ {
			active := rows[i*numSpeakers+s] > 0
			switch {
			case active && t.open[s] < 0:
				t.open[s] = f
			case !active && t.open[s] >= 0:
				t.close(s, f)
			}
		}
	}
	t.next = start + n
}

func (t *turnTracker) close(s, end int) {
	t.done = append(t.done, diarize.Turn{Start: float64(t.open[s]), End: float64(end), Speaker: s})
	t.open[s] = -1
}

// turns closes what is still open at the end of the recording and returns
// every turn in seconds, ordered by start, with the speakers renumbered from 0
// in order of first appearance.
func (t *turnTracker) turns(duration float64) []diarize.Turn {
	end := min(t.next, t.valid)
	for s := range t.open {
		if t.open[s] >= 0 {
			t.close(s, end)
		}
	}
	if len(t.done) == 0 {
		return nil
	}
	const frameSec = float64(hopLength) / sampleRate
	out := t.done
	for i := range out {
		out[i].Start *= frameSec
		// the last frame's 10 ms may run past the final sample
		out[i].End = min(out[i].End*frameSec, duration)
	}
	slices.SortFunc(out, func(a, b diarize.Turn) int {
		if a.Start != b.Start {
			if a.Start < b.Start {
				return -1
			}
			return 1
		}
		return a.Speaker - b.Speaker
	})
	// The model numbers speakers by arrival, but a channel can stay below the
	// threshold throughout, or a later channel can cross it first.
	remap := map[int]int{}
	for i := range out {
		id, ok := remap[out[i].Speaker]
		if !ok {
			id = len(remap)
			remap[out[i].Speaker] = id
		}
		out[i].Speaker = id
	}
	return out
}
