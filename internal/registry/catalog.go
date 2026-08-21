package registry

import (
	_ "embed"

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
