// Package registry knows what models exist, where they come from and what they
// can do. It is the only place that reads model.yaml and the catalog.
package registry

import (
	"regexp"

	"github.com/usunrise88/nanoasr/internal/core"
)

// idPattern also guards path construction: a model id becomes a directory name,
// so anything outside this set is rejected before it can traverse anywhere.
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

// Manifest describes one model. It lives next to the weights as model.yaml and
// is also what the embedded catalog serves.
type Manifest struct {
	ID          string   `yaml:"id" json:"id"`
	Revision    string   `yaml:"revision" json:"revision"`
	Family      string   `yaml:"family" json:"family"`
	DisplayName string   `yaml:"display_name" json:"display_name"`
	Languages   []string `yaml:"languages" json:"languages"`
	SampleRate  int      `yaml:"sample_rate" json:"sample_rate"`
	// ModelingUnit drives word assembly: bpe | cjkchar | char | cjkchar+bpe.
	ModelingUnit string `yaml:"modeling_unit" json:"modeling_unit"`

	Files        map[string]string `yaml:"files" json:"files"`
	Capabilities core.Capabilities `yaml:"capabilities" json:"capabilities"`
	Runtime      Runtime           `yaml:"runtime" json:"runtime"`
	Resources    Resources         `yaml:"resources" json:"resources"`
	Source       Source            `yaml:"source" json:"source"`
	License      string            `yaml:"license" json:"license"`
	Notes        string            `yaml:"notes,omitempty" json:"notes,omitempty"`
}

type Runtime struct {
	NumThreads     int     `yaml:"num_threads" json:"num_threads"`
	DecodingMethod string  `yaml:"decoding_method" json:"decoding_method"`
	MaxActivePaths int     `yaml:"max_active_paths" json:"max_active_paths"`
	BlankPenalty   float32 `yaml:"blank_penalty" json:"blank_penalty"`
}

type Resources struct {
	// ApproxRSSMB lets the pool decide whether a model fits before paying to
	// download and load it.
	ApproxRSSMB int `yaml:"approx_rss_mb" json:"approx_rss_mb"`
}

type Source struct {
	URL       string `yaml:"url" json:"url"`
	SHA256    string `yaml:"sha256" json:"sha256"`
	SizeBytes int64  `yaml:"size_bytes" json:"size_bytes"`
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
	if m.SampleRate == 0 {
		return core.Errorf(core.CodeInvalidRequest, "model %s: sample_rate is required", m.ID)
	}
	if len(m.Files) == 0 {
		return core.Errorf(core.CodeInvalidRequest, "model %s: files is empty", m.ID)
	}
	if m.Source.URL != "" && m.Source.SHA256 == "" {
		// A downloadable model without a checksum is a supply-chain hole, not
		// a convenience.
		return core.Errorf(core.CodeInvalidRequest,
			"model %s: source.sha256 is required when source.url is set", m.ID)
	}
	return nil
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
