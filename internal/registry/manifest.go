// Package registry knows what models exist, where they come from and what they
// can do. It is the only place that reads model.yaml and the catalog.
package registry

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Model roles. An empty kind means ASR, so existing manifests keep working.
const (
	KindASR          = "asr"
	KindVAD          = "vad"
	KindPunctuation  = "punctuation"
	KindSegmentation = "segmentation"
	KindEmbedding    = "embedding"
)

// idPattern also guards path construction: a model id becomes a directory name,
// so anything outside this set is rejected before it can traverse anywhere.
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

// Manifest describes one model. It lives next to the weights as model.yaml and
// is also what the embedded catalog serves.
type Manifest struct {
	ID       string `yaml:"id" json:"id"`
	Revision string `yaml:"revision" json:"revision"`
	// Kind separates the model roles the server hosts. Everything except ASR
	// is a supporting model — VAD, punctuation, diarization — and they have no
	// acoustic front end of their own to describe.
	Kind        string   `yaml:"kind" json:"kind"`
	Family      string   `yaml:"family" json:"family"`
	DisplayName string   `yaml:"display_name,omitempty" json:"display_name"`
	Languages   []string `yaml:"languages,omitempty" json:"languages"`
	SampleRate  int      `yaml:"sample_rate" json:"sample_rate"`
	// ModelingUnit drives word assembly: bpe | cjkchar | char | cjkchar+bpe.
	ModelingUnit string `yaml:"modeling_unit,omitempty" json:"modeling_unit"`

	Files    map[string]string `yaml:"files" json:"files"`
	Features Features          `yaml:"features" json:"features"`

	// Capabilities are deliberately absent: what a model can produce follows
	// from its family, and a manifest that claimed otherwise would be a lie
	// the pipeline discovers at runtime regardless.
	Runtime   Runtime   `yaml:"runtime,omitempty" json:"runtime"`
	Resources Resources `yaml:"resources,omitempty" json:"resources"`
	Source    Source    `yaml:"source,omitempty" json:"source"`
	License   string    `yaml:"license,omitempty" json:"license"`
	Notes     string    `yaml:"notes,omitempty" json:"notes,omitempty"`
}

// Features describes the acoustic front end the model was trained with.
//
// How much this matters depends on the family, and the honest answer was
// measured rather than assumed: NeMo models carry their own front end and
// ignore this value entirely — GigaAM v2 CTC transcribes correctly even when
// told 13 mel bins. Families whose extractor sherpa-onnx configures externally
// do use it, and there a mismatch does not fail, it degrades the transcript.
//
// Stating it is therefore required but cheap. `nanoasr models inspect --probe`
// reports which case a given model falls into.
type Features struct {
	SampleRate int `yaml:"sample_rate" json:"sample_rate"`
	Dim        int `yaml:"dim" json:"dim"`
}

type Runtime struct {
	NumThreads int `yaml:"num_threads,omitempty" json:"num_threads"`
	// ModelType is passed to sherpa-onnx to skip model-type sniffing at load.
	ModelType      string  `yaml:"model_type,omitempty" json:"model_type"`
	DecodingMethod string  `yaml:"decoding_method,omitempty" json:"decoding_method"`
	MaxActivePaths int     `yaml:"max_active_paths,omitempty" json:"max_active_paths"`
	BlankPenalty   float32 `yaml:"blank_penalty,omitempty" json:"blank_penalty"`
}

type Resources struct {
	// ApproxRSSMB lets the pool decide whether a model fits before paying to
	// download and load it.
	ApproxRSSMB int `yaml:"approx_rss_mb,omitempty" json:"approx_rss_mb"`
}

type Source struct {
	URL       string `yaml:"url,omitempty" json:"url"`
	SHA256    string `yaml:"sha256,omitempty" json:"sha256"`
	SizeBytes int64  `yaml:"size_bytes,omitempty" json:"size_bytes"`
}

// Key is the cache identity of a specific revision of a model.
func (m Manifest) Key() string {
	if m.Revision == "" {
		return m.ID
	}
	return m.ID + "@" + m.Revision
}

// Validate rejects a manifest before anything is downloaded or loaded.
func (m Manifest) Validate() error {
	if !idPattern.MatchString(m.ID) {
		return core.Errorf(core.CodeInvalidRequest,
			"model id %q must match [a-zA-Z0-9._-]{1,64}", m.ID)
	}
	if m.Family == "" {
		return core.Errorf(core.CodeInvalidRequest, "model %s: family is required", m.ID)
	}
	switch m.EffectiveKind() {
	case KindASR, KindVAD, KindPunctuation, KindSegmentation, KindEmbedding:
	default:
		return core.Errorf(core.CodeInvalidRequest,
			"model %s: unknown kind %q", m.ID, m.Kind)
	}
	if m.SampleRate == 0 {
		return core.Errorf(core.CodeInvalidRequest, "model %s: sample_rate is required", m.ID)
	}
	if len(m.Files) == 0 {
		return core.Errorf(core.CodeInvalidRequest, "model %s: files is empty", m.ID)
	}
	if m.EffectiveKind() == KindASR && m.Features.Dim <= 0 {
		return core.Errorf(core.CodeInvalidRequest,
			"model %s: features.dim must be stated (80 for most models, 64 for GigaAM); "+
				"use `nanoasr models inspect --probe` to find the right value", m.ID)
	}
	for role, name := range m.Files {
		if _, err := m.FilePath("/", role); err != nil {
			return core.Errorf(core.CodeInvalidRequest,
				"model %s: file %q has an unusable path %q", m.ID, role, name)
		}
	}
	if m.Source.URL != "" && m.Source.SHA256 == "" {
		// A downloadable model without a checksum is a supply-chain hole, not
		// a convenience.
		return core.Errorf(core.CodeInvalidRequest,
			"model %s: source.sha256 is required when source.url is set", m.ID)
	}
	return nil
}

// EffectiveKind defaults an unset kind to ASR.
func (m Manifest) EffectiveKind() string {
	if m.Kind == "" {
		return KindASR
	}
	return m.Kind
}

// FeatureSampleRate is the rate the front end expects, defaulting to the
// model's own sample rate when the manifest does not separate them.
func (m Manifest) FeatureSampleRate() int {
	if m.Features.SampleRate > 0 {
		return m.Features.SampleRate
	}
	return m.SampleRate
}

// FilePath resolves one manifest file role against the model directory.
//
// Manifests can arrive from a downloaded archive, so a file name is untrusted
// input: absolute paths and anything climbing out of dir are refused here
// rather than at the point of opening.
func (m Manifest) FilePath(dir, role string) (string, error) {
	name := m.Files[role]
	if name == "" {
		return "", core.Errorf(core.CodeInvalidRequest,
			"model %s: manifest has no file for %q", m.ID, role)
	}
	if filepath.IsAbs(name) || strings.ContainsRune(name, '\\') {
		return "", core.Errorf(core.CodeInvalidRequest,
			"model %s: file %q must be a relative path inside the model directory", m.ID, role)
	}

	// Check the name itself before joining: Join cleans "../.." away when the
	// base is the filesystem root, which would let an escape through exactly
	// the validation meant to catch it.
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", core.Errorf(core.CodeInvalidRequest,
			"model %s: file %q escapes the model directory", m.ID, role)
	}

	// filepath.Rel is the reliable containment check: a prefix comparison gets
	// the root directory wrong, where Clean("/") + separator is "//".
	cleanDir := filepath.Clean(dir)
	full := filepath.Join(cleanDir, clean)
	rel, err := filepath.Rel(cleanDir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", core.Errorf(core.CodeInvalidRequest,
			"model %s: file %q escapes the model directory", m.ID, role)
	}
	return full, nil
}

// OptionalFilePath is FilePath for roles that may legitimately be absent.
func (m Manifest) OptionalFilePath(dir, role string) string {
	if m.Files[role] == "" {
		return ""
	}
	path, err := m.FilePath(dir, role)
	if err != nil {
		return ""
	}
	return path
}

// Supports reports whether the model claims this language.
func (m Manifest) Supports(lang string) bool {
	if lang == "" {
		return true
	}
	for _, l := range m.Languages {
		if l == lang || l == "multi" {
			return true
		}
	}
	return false
}
