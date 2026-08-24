// Package config defines every knob NanoASR has, plus the defaults it computes
// when the operator does not know the target hardware yet (SPEC §12.3).
//
// Precedence: flags > NANOASR_* env > YAML file > computed defaults.
// The server starts with no configuration file at all.
package config

type Config struct {
	Server      Server      `yaml:"server"`
	Auth        Auth        `yaml:"auth"`
	API         API         `yaml:"api"`
	UI          UI          `yaml:"ui"`
	Audio       Audio       `yaml:"audio"`
	VAD         VAD         `yaml:"vad"`
	ASR         ASR         `yaml:"asr"`
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
}

type API struct {
	Dialects []string `yaml:"dialects"`
	CORS     CORS     `yaml:"cors"`
}

type CORS struct {
	Enabled bool     `yaml:"enabled"`
	Origins []string `yaml:"origins"`
}

type UI struct {
	Enabled     bool   `yaml:"enabled"`
	Path        string `yaml:"path"`
	RequireAuth bool   `yaml:"require_auth"`
}

type Audio struct {
	// FFmpegPath may be empty: then only WAV/PCM inputs are accepted and
	// everything else fails with unsupported_media_type.
	FFmpegPath       string   `yaml:"ffmpeg_path"`
	FFmpegTimeout    Duration `yaml:"ffmpeg_timeout"`
	MaxDuration      Duration `yaml:"max_duration"`
	TargetSampleRate int      `yaml:"target_sample_rate"`
	ChannelMode      string   `yaml:"channel_mode"`
	Denoise          bool     `yaml:"denoise"`
}

type VAD struct {
	Enabled      bool    `yaml:"enabled"`
	Model        string  `yaml:"model"`
	Threshold    float32 `yaml:"threshold"`
	MinSilenceMS int     `yaml:"min_silence_ms"`
	MinSpeechMS  int     `yaml:"min_speech_ms"`
	MaxSpeechSec float32 `yaml:"max_speech_sec"`
}

type ASR struct {
	ModelsDir         string   `yaml:"models_dir"`
	DefaultModel      string   `yaml:"default_model"`
	MaxResidentModels int      `yaml:"max_resident_models"`
	MaxModelRSSMB     int      `yaml:"max_model_rss_mb"`
	InferenceSlots    int      `yaml:"inference_slots"`
	NumThreads        int      `yaml:"num_threads"`
	IdleTTL           Duration `yaml:"idle_ttl"`
	AcquireTimeout    Duration `yaml:"acquire_timeout"`
	Batch             Batch    `yaml:"batch"`
}

type Batch struct {
	MaxSize    int `yaml:"max_size"`
	MaxSeconds int `yaml:"max_seconds"`
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
	Punctuation Punctuation `yaml:"punctuation"`
	ITN         ITN         `yaml:"itn"`
}

type Punctuation struct {
	Enabled bool   `yaml:"enabled"`
	Model   string `yaml:"model"`
}

type ITN struct {
	Enabled bool   `yaml:"enabled"`
	Locale  string `yaml:"locale"`
}

type Diarization struct {
	Enabled           bool       `yaml:"enabled"`
	SegmentationModel string     `yaml:"segmentation_model"`
	EmbeddingModel    string     `yaml:"embedding_model"`
	Clustering        Clustering `yaml:"clustering"`
}

type Clustering struct {
	NumClusters int     `yaml:"num_clusters"`
	Threshold   float32 `yaml:"threshold"`
}

type Storage struct {
	DBPath  string `yaml:"db_path"`
	TempDir string `yaml:"temp_dir"`
	// KeepAudioTTL of 0 means uploaded audio never outlives the active job.
	// This is the default and the reason the UI plays the local File instead
	// of asking the server for audio (SPEC §2.1).
	KeepAudioTTL Duration `yaml:"keep_audio_ttl"`
}

type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"` // json | text
}
