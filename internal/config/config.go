// Package config defines every knob NanoASR has, plus the defaults it computes
// when the operator does not know the target hardware yet (SPEC §12.3).
//
// Precedence: flags > NANOASR_* env > YAML file > computed defaults.
// The server starts with no configuration file at all.
package config

import "strings"

type Config struct {
	Server      Server      `yaml:"server"`
	Auth        Auth        `yaml:"auth"`
	API         API         `yaml:"api"`
	UI          UI          `yaml:"ui"`
	Audio       Audio       `yaml:"audio"`
	VAD         VAD         `yaml:"vad"`
	ASR         ASR         `yaml:"asr"`
	Realtime    Realtime    `yaml:"realtime"`
	Registry    Registry    `yaml:"registry"`
	Jobs        Jobs        `yaml:"jobs"`
	PostProc    PostProc    `yaml:"postproc"`
	Diarization Diarization `yaml:"diarization"`
	Storage     Storage     `yaml:"storage"`
	Log         Log         `yaml:"log"`
}

type Server struct {
	Addr              string   `yaml:"addr"`
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	MaxUploadBytes    int64    `yaml:"max_upload_bytes"`
	ShutdownGrace     Duration `yaml:"shutdown_grace"`
}

// Auth in mode "open" is accepted only when Server.Addr binds a loopback
// address; otherwise the server refuses to start.
type Auth struct {
	Mode string   `yaml:"mode"` // apikey | open
	Keys []APIKey `yaml:"keys"`
}

// APIKey is one credential. It can be written as a bare string when nothing
// but the secret matters, or as a mapping when the key needs a name or
// administrative rights:
//
//	keys:
//	  - sk-readonly-abcdef0123456789
//	  - name: ci
//	    key: sha256:9f86d0818...
//	    admin: true
type APIKey struct {
	Name string `yaml:"name"`
	// Key is the secret itself, or "sha256:<hex>" to keep the plaintext out
	// of the configuration file.
	Key   string `yaml:"key"`
	Admin bool   `yaml:"admin"`
	// RPS caps this key's request rate. Zero means unlimited, which is the
	// right default for a key an operator issued to themselves.
	RPS float64 `yaml:"rps"`
	// Priority is "interactive" or "batch" (the default). Interactive work
	// overtakes a batch backlog.
	//
	// It belongs to the key rather than to the request because a request
	// parameter would let every client call itself urgent. The key the test UI
	// uses is the one to mark interactive.
	Priority string `yaml:"priority"`
}

// Interactive reports whether this key's jobs overtake the batch backlog.
func (k APIKey) Interactive() bool { return k.Priority == PriorityInteractive }

// Job priorities a key may be given.
const (
	PriorityBatch       = "batch"
	PriorityInteractive = "interactive"
)

// Dialect names api.dialects accepts. They are declared here rather than only
// in the packages that register them so that configuration validation can name
// one without importing the HTTP layer.
const (
	DialectOpenAI   = "openai"
	DialectNative   = "native"
	DialectEra      = "era"
	DialectRealtime = "realtime"
)

type API struct {
	Dialects []string `yaml:"dialects"`
}

// DialectEnabled reports whether this dialect is mounted. Several settings are
// only meaningful when one particular dialect is serving — realtime.model is
// required exactly when the realtime dialect is on — and validating them
// unconditionally would refuse configurations that are perfectly fine.
func (a API) DialectEnabled(name string) bool {
	for _, d := range a.Dialects {
		if strings.TrimSpace(d) == name {
			return true
		}
	}
	return false
}

// UI has no require_auth knob. Whether a key is needed is not a UI setting: the
// SPA finds out by getting 401 from the first API call it makes, which is the
// same answer the server would have had to publish anyway, arriving sooner.
type UI struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

type Audio struct {
	// FFmpegPath may be empty: then only WAV/PCM inputs are accepted and
	// everything else fails with unsupported_media_type.
	FFmpegPath       string   `yaml:"ffmpeg_path"`
	FFmpegTimeout    Duration `yaml:"ffmpeg_timeout"`
	MaxDuration      Duration `yaml:"max_duration"`
	TargetSampleRate int      `yaml:"target_sample_rate"`
	ChannelMode      string   `yaml:"channel_mode"`
	// MaxSplitChannels bounds what channel_mode: split will accept. Split
	// decodes every channel in full, so memory scales with the channel count —
	// a six-channel field recording is not a pair of telephony legs, and
	// refusing it is better than an OOM half an hour into the file.
	MaxSplitChannels int `yaml:"max_split_channels"`
	// MaxDecodedBytes caps decoded PCM across all channels. The duration limit
	// alone stopped being sufficient once one file could produce N channels of
	// it. 0 → derived in Autotune from max_duration and max_split_channels.
	MaxDecodedBytes int64 `yaml:"max_decoded_bytes"`
}

type VAD struct {
	Enabled      bool    `yaml:"enabled"`
	Model        string  `yaml:"model"`
	Threshold    float32 `yaml:"threshold"`
	MinSilenceMS int     `yaml:"min_silence_ms"`
	MinSpeechMS  int     `yaml:"min_speech_ms"`
	MaxSpeechSec float32 `yaml:"max_speech_sec"`
}

// Execution providers asr.provider accepts.
//
// There is no "auto": a server that was asked for the GPU and quietly used the
// CPU is the failure this whole knob exists to prevent, and a server that was
// asked for the CPU has nothing to detect.
const (
	ProviderCPU  = "cpu"
	ProviderCUDA = "cuda"
)

type ASR struct {
	ModelsDir string `yaml:"models_dir"`
	// Provider is the onnxruntime execution provider: "cpu" or "cuda".
	//
	// It is one setting for every recogniser rather than one per model,
	// because the provider is a property of the native libraries the process
	// loaded, not of a set of weights. "cuda" therefore requires the GPU build
	// of sherpa-onnx on the library path; the server checks that at startup and
	// refuses to run rather than fall back to the CPU without saying so, which
	// is what onnxruntime does on its own.
	//
	// VAD stays on the CPU whatever this says — see sherpa.ProviderFor.
	Provider          string   `yaml:"provider"`
	DefaultModel      string   `yaml:"default_model"`
	MaxResidentModels int      `yaml:"max_resident_models"`
	MaxModelRSSMB     int      `yaml:"max_model_rss_mb"`
	InferenceSlots    int      `yaml:"inference_slots"`
	NumThreads        int      `yaml:"num_threads"`
	IdleTTL           Duration `yaml:"idle_ttl"`
	AcquireTimeout    Duration `yaml:"acquire_timeout"`
	Batch             Batch    `yaml:"batch"`
	Variants          Variants `yaml:"variants"`
}

// Variants governs per-request recogniser configuration.
//
// Hotwords, decoding_method and max_active_paths cannot be changed on a loaded
// recogniser: sherpa-onnx settles them at construction, and the Go binding
// exposes no per-stream override. Honouring them per request therefore means
// admitting a second resident copy of the model, so the cost is memory and the
// operator decides whether to pay it. Max 0 — the default — means the request
// is answered with the model's configured behaviour and a warning saying so.
type Variants struct {
	Max int `yaml:"max"`
	// AllowHotwords is read and ignored.
	//
	// It was a second switch for something postproc.hotwords.enabled already
	// decides, and nothing ever consulted it — a server with it false and
	// postproc.hotwords.enabled true applied hotwords, as the documentation
	// said it would. It stays in the struct because configurations in the
	// field set it and KnownFields turns an unknown key into a startup
	// failure: removing it would stop those servers from booting to retire a
	// field that never did anything.
	AllowHotwords bool `yaml:"allow_hotwords"`
}

type Batch struct {
	MaxSize    int `yaml:"max_size"`
	MaxSeconds int `yaml:"max_seconds"`
}

// Realtime configures the streaming recogniser behind the websocket dialect.
//
// It is a separate section from ASR because almost nothing carries over: a
// streaming model is a different export of a different architecture, it is
// resident for the life of the server rather than pooled, and what bounds it is
// concurrent connections rather than queued jobs.
type Realtime struct {
	// Model is the streaming model every session uses. One model, not a pool:
	// a realtime session holds decoder state for its whole life, so admitting a
	// second model means a second resident copy with no way to evict it while
	// anyone is connected. A client asking for a different one is told so.
	Model string `yaml:"model"`
	// MaxSessions bounds concurrent connections. 0 → derived in Autotune.
	MaxSessions int `yaml:"max_sessions"`
	// MaxSessionDuration is the hard ceiling on one connection. A realtime
	// socket has no natural end, and without this a forgotten browser tab
	// holds a decoder slot for as long as the process lives.
	MaxSessionDuration Duration `yaml:"max_session_duration"`
	// IdleTimeout closes a session that has sent no audio for this long. A
	// client that is genuinely streaming a microphone sends silence, so this
	// catches a dead peer rather than a quiet speaker.
	IdleTimeout Duration `yaml:"idle_timeout"`
	// PartialInterval is the floor on the gap between two delta events for one
	// utterance. Partial hypotheses change on every decoded chunk, and a
	// client that renders each one is doing eighty DOM updates a second.
	PartialInterval Duration `yaml:"partial_interval"`
	// MaxBufferedSeconds bounds the audio one session may have queued for
	// decoding. Reaching it means the server cannot keep up with the stream,
	// which is reported to the client and closes the session: silently
	// dropping audio would produce a transcript with a hole in it and no
	// indication that anything was lost.
	MaxBufferedSeconds int `yaml:"max_buffered_seconds"`
	// NumThreads is the decoder's intra-op thread count. 0 → asr.num_threads.
	NumThreads int `yaml:"num_threads"`
	// BatchSize is how many sessions are decoded in one call. Batching is what
	// makes a GPU worth having here: one stream cannot fill it.
	BatchSize      int      `yaml:"batch_size"`
	DecodingMethod string   `yaml:"decoding_method"`
	MaxActivePaths int      `yaml:"max_active_paths"`
	Endpoint       Endpoint `yaml:"endpoint"`
}

// Endpoint is sherpa-onnx's endpoint detection, which is what decides where one
// utterance ends and the next begins. The three rules are OR-ed, and the names
// are sherpa's own so that its documentation applies unchanged.
type Endpoint struct {
	Enabled bool `yaml:"enabled"`
	// Rule1MinTrailingSilence ends an utterance after this much silence when
	// nothing has been decoded yet, in seconds.
	Rule1MinTrailingSilence float32 `yaml:"rule1_min_trailing_silence"`
	// Rule2MinTrailingSilence ends it after this much silence following
	// decoded speech. This is the one a caller feels: it is the pause after
	// which the final transcript arrives.
	Rule2MinTrailingSilence float32 `yaml:"rule2_min_trailing_silence"`
	// Rule3MinUtteranceLength ends it after this many seconds regardless, so a
	// monologue still produces results.
	Rule3MinUtteranceLength float32 `yaml:"rule3_min_utterance_length"`
}

type Registry struct {
	AllowDownload       bool     `yaml:"allow_download"`
	CatalogURL          string   `yaml:"catalog_url"`
	Mirrors             []string `yaml:"mirrors"`
	DownloadConcurrency int      `yaml:"download_concurrency"`
	StrictLicense       bool     `yaml:"strict_license"`
}

type Jobs struct {
	QueueSize     int `yaml:"queue_size"`
	MaxConcurrent int `yaml:"max_concurrent"`
	// MaxQueuedBytes caps the audio the queue holds on disk at once.
	//
	// It is a second limit beside QueueSize because the two bound different
	// things: a hundred queued slots at the hundred-megabyte upload limit is
	// ten gigabytes of disk, and a server that accepts work it has nowhere to
	// put fails later and worse than one that answers 429 now.
	MaxQueuedBytes    int64    `yaml:"max_queued_bytes"`
	MaxProcessingTime Duration `yaml:"max_processing_time"`
	HistoryTTL        Duration `yaml:"history_ttl"`
	// WebhookSecret signs deliveries. Empty means unsigned, which is logged
	// at startup rather than passed over in silence.
	WebhookSecret string `yaml:"webhook_secret"`
	// WebhookAllowPrivate turns off the address check that keeps webhook_url
	// from reaching into the network the server sits in. It exists for a
	// developer whose receiver is on localhost; Validate refuses it anywhere
	// a loopback bind would not also be refused.
	WebhookAllowPrivate bool `yaml:"webhook_allow_private"`
}

type PostProc struct {
	Punctuation Punctuation    `yaml:"punctuation"`
	ITN         ITN            `yaml:"itn"`
	Hotwords    HotwordsPolicy `yaml:"hotwords"`
}

// HotwordsPolicy is the server-side half of hotword biasing. The words
// themselves are always per request; what the server decides is whether it will
// spend a model instance on them and what score to apply when the caller does
// not say. The OpenAI dialect maps prompt → hotwords without a score, so a
// default is not optional.
type HotwordsPolicy struct {
	Enabled      bool    `yaml:"enabled"`
	DefaultScore float32 `yaml:"default_score"`
	// MaxPhrases bounds one stored dictionary. Accuracy gives out long before
	// this does — the longer a bias list, the more often it fires in the wrong
	// place — so it guards against a runaway import rather than describing a
	// sensible list.
	MaxPhrases int `yaml:"max_phrases"`
}

type Punctuation struct {
	Enabled bool   `yaml:"enabled"`
	Model   string `yaml:"model"`
}

type ITN struct {
	Enabled bool   `yaml:"enabled"`
	Locale  string `yaml:"locale"`
}

// Diarization backends.
const (
	// DiarizationSortformer is Nemotron-3-Diarization: one end-to-end model
	// that labels up to eight speakers with no clustering.
	DiarizationSortformer = "sortformer"
	// DiarizationSherpa is sherpa-onnx's pipeline: pyannote segmentation,
	// speaker embeddings and clustering. It is the one that can be told the
	// exact number of speakers.
	DiarizationSherpa = "sherpa"
)

type Diarization struct {
	Enabled bool `yaml:"enabled"`
	// Backend is sortformer or sherpa. The keys below it apply to one each.
	Backend string `yaml:"backend"`

	// Model is the sortformer backend's catalog id.
	Model string `yaml:"model"`

	// The sherpa backend's models and clustering.
	SegmentationModel string     `yaml:"segmentation_model"`
	EmbeddingModel    string     `yaml:"embedding_model"`
	Clustering        Clustering `yaml:"clustering"`
	// MinDurationOn and MinDurationOff smooth the segmentation output: how
	// short a turn may be before it is discarded, and how short a gap may be
	// before the two turns around it are joined. SPEC §5.7 names both; they
	// had no key until M5. Sherpa only: Sortformer's output is thresholded as
	// its reference does, and diarize.Split already smooths at the word level.
	MinDurationOn  float32 `yaml:"min_duration_on"`
	MinDurationOff float32 `yaml:"min_duration_off"`
}

type Clustering struct {
	NumClusters int     `yaml:"num_clusters"`
	Threshold   float32 `yaml:"threshold"`
}

// Storage has no keep_audio_ttl knob, and that is the point.
//
// Uploaded audio outlives its job by exactly nothing: internal/spool deletes it
// when the job reaches a terminal state, and startup cleanup deletes whatever a
// crash left behind. Zero was the only supported value of that setting, so
// removing it turns decision §15 from a default into a property (SPEC §2.1).
type Storage struct {
	DBPath  string `yaml:"db_path"`
	TempDir string `yaml:"temp_dir"`
}

type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"` // json | text
}
