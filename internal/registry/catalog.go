package registry

import (
	_ "embed"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// catalogYAML is the built-in model catalog. Operators can override it with
// registry.catalog_url to point at a private mirror in a closed network.
//
//go:embed catalog.yaml
var catalogYAML []byte

type catalogFile struct {
	Version int        `yaml:"version"`
	Models  []Manifest `yaml:"models"`
}

// Builtin parses the embedded catalog.
func Builtin() ([]Manifest, error) {
	var f catalogFile
	if err := yaml.Unmarshal(catalogYAML, &f); err != nil {
		return nil, err
	}
	return f.Models, nil
}

// CoreMLSuffix is how the catalog names the CoreML build of a model: the id
// of the model it was converted from, with this added.
const CoreMLSuffix = "-coreml"

var (
	builtinOnce sync.Once
	builtinIDs  map[string]bool
)

// CoreMLBuild returns the id of a model's CoreML build in the built-in
// catalog, or "" when it has none.
func CoreMLBuild(id string) string {
	builtinOnce.Do(func() {
		builtinIDs = map[string]bool{}
		ms, err := Builtin()
		if err != nil {
			return
		}
		for _, m := range ms {
			builtinIDs[m.ID] = true
		}
	})
	if strings.HasSuffix(id, CoreMLSuffix) {
		return id
	}
	if builtinIDs[id+CoreMLSuffix] {
		return id + CoreMLSuffix
	}
	return ""
}
