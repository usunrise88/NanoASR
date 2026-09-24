package sortformer

import (
	"context"
	"fmt"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Model dimensions the graphs are exported with. They are properties of the
// checkpoint, checked against the model's config.json when it is loaded, and
// constants here because every buffer in the chunk loop is sized by them.
const (
	melBins     = 128 // log-mel features per 10 ms frame
	subsampling = 8   // mel frames per encoder frame
	hiddenSize  = 512 // encoder embedding width
	numSpeakers = 8   // speaker channels the head predicts
)

// runner is the network, reduced to the two calls the chunk loop makes.
//
// It is an interface so that the loop and the speaker cache — the parts of the
// port most likely to be subtly wrong — can be tested against the reference
// with a fake network whose outputs are known exactly, without a model on disk.
type runner interface {
	// embed maps frames log-mel rows ([frames][melBins], row-major) to
	// frames/subsampling embeddings ([][hiddenSize]). frames must be a multiple
	// of subsampling: the caller zero-pads the last group, as the reference
	// does, so the graph never has to.
	embed(feats []float32, frames int) ([]float32, error)

	// step runs the encoder and head over frames embeddings and writes
	// frames*subsampling rows of numSpeakers logits into out.
	//
	// The graph takes no lengths and attends to everything it is given, so the
	// input must be exactly the frames that count: never padding.
	step(ctx context.Context, embeds []float32, frames int, out []float32) error

	Close() error
}

// ortRunner runs the two graphs through onnxruntime.
//
// One instance serves every concurrent job. onnxruntime's Run is thread-safe on
// a shared session, and the binding keeps all per-call state in locals.
//
// Measured on the fp32 graphs, four cores: the sessions take ~460 MB resident
// and one full step adds ~35 MB of activations, allocated by each Run. Two jobs
// on one session run at 0.76-0.81 of the throughput of two jobs on two
// sessions — concurrent Runs share the session's intra-op pool — which is not
// worth another 450 MB of weights per job.
type ortRunner struct {
	embedSess *ort.DynamicAdvancedSession
	stepSess  *ort.DynamicAdvancedSession
}

// sessionTuning is what every session gets.
type sessionTuning struct {
	// IntraOpThreads is the thread budget of one Run. The pipeline reserves
	// exactly this many slots from its governor around the diarize stage.
	IntraOpThreads int
	// NoSpinning stops idle onnxruntime workers from busy-waiting between
	// kernels. Spinning threads burn CPU outside the governor's accounting,
	// which is CPU sherpa's recognizers were counting on.
	NoSpinning bool
}

func newORTRunner(embedPath, stepPath string, tune sessionTuning) (*ortRunner, error) {
	if err := ensureEnvironment(); err != nil {
		return nil, err
	}
	opts, err := sessionOptions(tune)
	if err != nil {
		return nil, err
	}
	defer func() { _ = opts.Destroy() }()

	r := &ortRunner{}
	r.embedSess, err = ort.NewDynamicAdvancedSession(embedPath, []string{"features"}, []string{"embeds"}, opts)
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "cannot load %s", embedPath).WithCause(err)
	}
	r.stepSess, err = ort.NewDynamicAdvancedSession(stepPath, []string{"embeds"}, []string{"logits"}, opts)
	if err != nil {
		_ = r.embedSess.Destroy()
		return nil, core.Errorf(core.CodeInternal, "cannot load %s", stepPath).WithCause(err)
	}
	return r, nil
}

func sessionOptions(tune sessionTuning) (*ort.SessionOptions, error) {
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "cannot create onnxruntime session options").WithCause(err)
	}
	threads := tune.IntraOpThreads
	if threads < 1 {
		threads = 1
	}
	steps := []error{
		opts.SetIntraOpNumThreads(threads),
		// One graph, one Run at a time per call: inter-op parallelism would
		// only add threads the governor does not know about.
		opts.SetInterOpNumThreads(1),
		opts.SetExecutionMode(ort.ExecutionModeSequential),
	}
	if tune.NoSpinning {
		steps = append(steps, opts.AddSessionConfigEntry("session.intra_op.allow_spinning", "0"))
	}
	for _, e := range steps {
		if e != nil {
			_ = opts.Destroy()
			return nil, core.Errorf(core.CodeInternal, "cannot configure onnxruntime session").WithCause(e)
		}
	}
	return opts, nil
}

func (r *ortRunner) embed(feats []float32, frames int) ([]float32, error) {
	if frames%subsampling != 0 || len(feats) != frames*melBins {
		return nil, fmt.Errorf("embed: %d frames (%d values) is not a whole number of %d-frame groups",
			frames, len(feats), subsampling)
	}
	if frames == 0 {
		return nil, nil
	}
	in, err := ort.NewTensor(ort.NewShape(1, int64(frames), melBins), feats)
	if err != nil {
		return nil, err
	}
	defer func() { _ = in.Destroy() }()

	groups := frames / subsampling
	out := make([]float32, groups*hiddenSize)
	outT, err := ort.NewTensor(ort.NewShape(1, int64(groups), hiddenSize), out)
	if err != nil {
		return nil, err
	}
	defer func() { _ = outT.Destroy() }()

	if err := r.embedSess.Run([]ort.Value{in}, []ort.Value{outT}); err != nil {
		return nil, core.Errorf(core.CodeInternal, "embedding graph failed").WithCause(err)
	}
	return out, nil
}

func (r *ortRunner) step(ctx context.Context, embeds []float32, frames int, out []float32) error {
	if len(embeds) != frames*hiddenSize || len(out) != frames*subsampling*numSpeakers {
		return fmt.Errorf("step: %d frames do not match %d embedding values and %d logits",
			frames, len(embeds), len(out))
	}
	in, err := ort.NewTensor(ort.NewShape(1, int64(frames), hiddenSize), embeds)
	if err != nil {
		return err
	}
	defer func() { _ = in.Destroy() }()
	outT, err := ort.NewTensor(ort.NewShape(1, int64(frames*subsampling), numSpeakers), out)
	if err != nil {
		return err
	}
	defer func() { _ = outT.Destroy() }()

	// A step is ~0.6 s of CPU on four cores; a cancelled request should not
	// have to wait for it. Terminate makes the Run in flight return an error.
	runOpts, err := ort.NewRunOptions()
	if err != nil {
		return core.Errorf(core.CodeInternal, "cannot create onnxruntime run options").WithCause(err)
	}
	defer func() { _ = runOpts.Destroy() }()
	stop := context.AfterFunc(ctx, func() { _ = runOpts.Terminate() })
	defer stop()

	if err := r.stepSess.RunWithOptions([]ort.Value{in}, []ort.Value{outT}, runOpts); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return core.Errorf(core.CodeInternal, "diarization graph failed").WithCause(err)
	}
	return nil
}

func (r *ortRunner) Close() error {
	var first error
	for _, s := range []*ort.DynamicAdvancedSession{r.embedSess, r.stepSess} {
		if s == nil {
			continue
		}
		if err := s.Destroy(); err != nil && first == nil {
			first = err
		}
	}
	r.embedSess, r.stepSess = nil, nil
	return first
}
