package config

import (
	"crypto/rand"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Document is a configuration file held as a YAML tree rather than as a
// Config.
//
// Editing a config file by unmarshalling it into Config and marshalling it
// back would work exactly once: the second time, every comment the operator
// wrote would be gone, every key would have moved to struct order, and every
// default the loader filled in would have been written down as if it had been
// chosen. A yaml.Node keeps comments, order and absence, so `nanoasr key
// issue` changes one list and nothing else.
type Document struct {
	root yaml.Node
}

// ParseDocument reads a configuration file into an editable tree.
func ParseDocument(b []byte) (*Document, error) {
	d := &Document{}
	if err := yaml.Unmarshal(b, &d.root); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	// An empty file decodes to a zero node. Give it a document with an empty
	// mapping so the first edit has somewhere to go.
	if d.root.Kind == 0 {
		d.root = yaml.Node{
			Kind:    yaml.DocumentNode,
			Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}},
		}
	}
	if d.root.Kind != yaml.DocumentNode || len(d.root.Content) != 1 ||
		d.root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse config: expected a YAML mapping at the top level")
	}
	return d, nil
}

// Bytes renders the document back to YAML with the indentation the shipped
// configuration files use.
func (d *Document) Bytes() ([]byte, error) {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&d.root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(spaceTopLevel(b.String())), nil
}

// spaceTopLevel puts a blank line back in front of every top-level block.
//
// yaml.v3 keeps comments but not blank lines, so a file that came in as eight
// readable sections would go out as one wall of text — a real cost for a file
// whose whole job is to be read and edited by a person. The separators are the
// one piece of layout worth reconstructing, and where they belong is
// unambiguous: before each top-level key, above any comment that introduces it.
func spaceTopLevel(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines)+16)
	for _, line := range lines {
		if isTopLevelKey(line) {
			// Walk back over the comment block that belongs to this key.
			at := len(out)
			for at > 0 && strings.HasPrefix(out[at-1], "#") {
				at--
			}
			if at > 0 && strings.TrimSpace(out[at-1]) != "" {
				out = append(out, "")
				copy(out[at+1:], out[at:])
				out[at] = ""
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func isTopLevelKey(line string) bool {
	if line == "" || line[0] == ' ' || line[0] == '#' || line[0] == '-' {
		return false
	}
	name, _, found := strings.Cut(line, ":")
	return found && name != "" && !strings.ContainsAny(name, " \t")
}

// Keys returns the credentials the file declares, in file order.
func (d *Document) Keys() []APIKey {
	seq := d.find("auth", "keys")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil
	}
	out := make([]APIKey, 0, len(seq.Content))
	for _, n := range seq.Content {
		var k APIKey
		if err := n.Decode(&k); err != nil {
			continue // a malformed entry is the loader's complaint, not ours
		}
		out = append(out, k)
	}
	return out
}

// AddKey appends a credential. Names must be unique, because every other
// command addresses a key by its name and two keys called "ci" would make
// `nanoasr key remove ci` ambiguous in a file the operator cannot see.
func (d *Document) AddKey(k APIKey) error {
	if k.Key == "" {
		return fmt.Errorf("a key needs a secret")
	}
	for _, have := range d.Keys() {
		if k.Name != "" && have.Name == k.Name {
			return fmt.Errorf("a key named %q already exists", k.Name)
		}
		if have.Key == k.Key {
			return fmt.Errorf("that secret is already in the file")
		}
	}

	seq := d.ensure("auth", "keys")
	seq.Kind = yaml.SequenceNode
	seq.Tag = "!!seq"
	seq.Value = ""

	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	put := func(name, value, tag string) {
		entry.Content = append(entry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value})
	}
	if k.Name != "" {
		put("name", k.Name, "!!str")
	}
	put("key", k.Key, "!!str")
	if k.Admin {
		put("admin", "true", "!!bool")
	}
	if k.RPS > 0 {
		put("rps", yamlFloat(k.RPS), "!!float")
	}
	if k.Priority != "" {
		put("priority", k.Priority, "!!str")
	}
	seq.Content = append(seq.Content, entry)
	return nil
}

// RemoveKey drops the credential with this name, or — so that a key issued
// before names were used can still be revoked — the one whose secret matches.
func (d *Document) RemoveKey(nameOrSecret string) bool {
	seq := d.find("auth", "keys")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return false
	}
	for i, n := range seq.Content {
		var k APIKey
		if err := n.Decode(&k); err != nil {
			continue
		}
		if k.Name != nameOrSecret && k.Key != nameOrSecret {
			continue
		}
		seq.Content = append(seq.Content[:i:i], seq.Content[i+1:]...)
		return true
	}
	return false
}

// SetString sets a scalar at a dotted path, creating the intermediate mappings
// it needs. It exists so `nanoasr init` can point a rendered file at the
// directories this machine actually has.
func (d *Document) SetString(path, value string) {
	n := d.ensure(strings.Split(path, ".")...)
	n.Kind, n.Tag, n.Value, n.Content = yaml.ScalarNode, "!!str", value, nil
	n.Style = 0
}

// SetBool is SetString for a boolean, kept separate so the tag stays honest:
// a "true" written as a string is not a true the loader will accept.
func (d *Document) SetBool(path string, value bool) {
	n := d.ensure(strings.Split(path, ".")...)
	n.Kind, n.Tag, n.Content = yaml.ScalarNode, "!!bool", nil
	n.Value = "false"
	if value {
		n.Value = "true"
	}
	n.Style = 0
}

// find walks to a node, or returns nil if any step is missing.
func (d *Document) find(path ...string) *yaml.Node {
	n := d.root.Content[0]
	for _, want := range path {
		if n.Kind != yaml.MappingNode {
			return nil
		}
		next := (*yaml.Node)(nil)
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == want {
				next = n.Content[i+1]
				break
			}
		}
		if next == nil {
			return nil
		}
		n = next
	}
	return n
}

// ensure is find that creates what it does not find.
func (d *Document) ensure(path ...string) *yaml.Node {
	n := d.root.Content[0]
	for _, want := range path {
		if n.Kind != yaml.MappingNode {
			n.Kind, n.Tag, n.Value, n.Content = yaml.MappingNode, "!!map", "", nil
		}
		next := (*yaml.Node)(nil)
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == want {
				next = n.Content[i+1]
				break
			}
		}
		if next == nil {
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			n.Content = append(n.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: want}, next)
		}
		n = next
	}
	return n
}

// yamlFloat renders a rate without the trailing zeros %f would leave behind.
func yamlFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// KeyPrefix is what an issued secret starts with, so that a key found in a log
// or a paste is recognisable as one of ours.
const KeyPrefix = "sk-nanoasr-"

// keyAlphabet excludes the characters that get confused when a secret is read
// off a screen and typed back in: 0/O and 1/l/I.
const keyAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// NewKeySecret mints a credential with 32 characters out of a 56-character
// alphabet — about 186 bits, which is more than the sha256 the server hashes
// it into can distinguish anyway.
//
// Bytes that would fold unevenly onto the alphabet are drawn again rather than
// taken modulo: the bias would be small enough not to matter here, and
// rejecting is shorter than the comment explaining why it did not.
func NewKeySecret() (string, error) {
	const n = 32
	limit := byte(256 - 256%len(keyAlphabet))

	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generate an api key: %w", err)
		}
		for _, b := range buf {
			if b >= limit {
				continue
			}
			out = append(out, keyAlphabet[int(b)%len(keyAlphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return KeyPrefix + string(out), nil
}
