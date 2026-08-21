package core

import "context"

// Service is the only entry point into NanoASR business logic. API dialects
// depend on this interface and on nothing below it: they must never reach into
// the model pool, the registry or the queue directly.
type Service interface {
	// Transcribe runs the pipeline synchronously. Cancelling ctx aborts
	// decoding between batches rather than at the end of the file.
	Transcribe(ctx context.Context, req Request) (*Result, error)

	// Submit enqueues the request and returns immediately.
	Submit(ctx context.Context, req Request) (*Job, error)

	Job(ctx context.Context, id string) (*Job, error)
	ListJobs(ctx context.Context, f JobFilter) ([]Job, error)
	Cancel(ctx context.Context, id string) error

	// Watch streams job state transitions until the job is terminal or ctx is
	// done. The channel is closed on return.
	Watch(ctx context.Context, id string) (<-chan Job, error)
}

// ModelState is where a model sits in the download → load → ready lifecycle.
type ModelState string

const (
	ModelAbsent      ModelState = "absent"
	ModelDownloading ModelState = "downloading"
	ModelDownloaded  ModelState = "downloaded"
	ModelLoading     ModelState = "loading"
	ModelReady       ModelState = "ready"
	ModelDraining    ModelState = "draining"
)

// Capabilities says what a model can actually deliver. The pipeline consults it
// instead of assuming; a request for something absent produces a warning (or an
// error in strict mode).
type Capabilities struct {
	WordTimestamps     bool `json:"word_timestamps"`
	Confidence         bool `json:"confidence"`
	LanguageDetect     bool `json:"language_detect"`
	PunctuationBuiltin bool `json:"punctuation_builtin"`
}

// ModelInfo is the operator-facing view of one model.
type ModelInfo struct {
	ID          string `json:"id"`
	Revision    string `json:"revision"`
	DisplayName string `json:"display_name"`
	// Kind separates transcription models from the supporting ones — VAD,
	// punctuation, diarization — that share the same registry.
	Kind         string       `json:"kind"`
	Family       string       `json:"family"`
	Languages    []string     `json:"languages"`
	License      string       `json:"license"`
	State        ModelState   `json:"state"`
	Pinned       bool         `json:"pinned"`
	RefCount     int          `json:"ref_count"`
	RSSMB        int          `json:"rss_mb"`
	LastUsedUnix int64        `json:"last_used_unix,omitempty"`
	Capabilities Capabilities `json:"capabilities"`
}

// DownloadProgress is one tick of a model download, streamed to the UI.
type DownloadProgress struct {
	ModelID    string  `json:"model_id"`
	Downloaded int64   `json:"downloaded"`
	Total      int64   `json:"total"`
	Percent    float64 `json:"percent"`
	Done       bool    `json:"done"`
	Err        string  `json:"error,omitempty"`
}

// ModelService is the admin surface. It is separate from Service so a dialect
// can expose transcription without exposing model management.
type ModelService interface {
	List(ctx context.Context) ([]ModelInfo, error)
	Catalog(ctx context.Context) ([]ModelInfo, error)

	Download(ctx context.Context, id string) (<-chan DownloadProgress, error)
	Load(ctx context.Context, id string) error
	Unload(ctx context.Context, id string) error
	Pin(ctx context.Context, id string, pinned bool) error

	// Reload loads revision alongside the running instance and swaps the
	// pointer once the new one is warm. In-flight requests finish on the old
	// instance; nobody observes a half-loaded model.
	Reload(ctx context.Context, id, revision string) error
}

// Observer receives pipeline events. The default implementation does nothing.
//
// Metrics and tracing are out of scope for v1 by explicit decision (SPEC §2.2).
// This interface exists so that adding them later is wiring, not surgery: every
// pipeline stage already reports through it.
type Observer interface {
	StageStarted(ctx context.Context, jobID, stage string)
	StageFinished(ctx context.Context, jobID, stage string, ms int64, err error)
	QueueDepth(n int)
	ModelEvent(id string, from, to ModelState)
}

// NopObserver discards everything.
type NopObserver struct{}

func (NopObserver) StageStarted(context.Context, string, string)                {}
func (NopObserver) StageFinished(context.Context, string, string, int64, error) {}
func (NopObserver) QueueDepth(int)                                              {}
func (NopObserver) ModelEvent(string, ModelState, ModelState)                   {}

var _ Observer = NopObserver{}
