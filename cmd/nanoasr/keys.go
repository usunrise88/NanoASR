package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/usunrise88/nanoasr/internal/config"
)

// keyCommand manages the credentials in a configuration file.
//
//	nanoasr key list
//	nanoasr key issue NAME [-admin] [-rps N] [-interactive] [-hash]
//	nanoasr key remove NAME
//
// The file is edited as a YAML document rather than loaded into Config and
// written back, so an operator's comments, ordering and omissions survive the
// edit — see config.Document.
func keyCommand(args []string) error {
	sub, args := takePositional(args, "list")

	fs := flag.NewFlagSet("key "+sub, flag.ExitOnError)
	path := fs.String("config", configPathDefault(), "path to nanoasr.yaml")
	admin := fs.Bool("admin", false, "issue: grant administrative rights")
	interactive := fs.Bool("interactive", false, "issue: this key's jobs overtake the batch backlog")
	rps := fs.Float64("rps", 0, "issue: requests per second, 0 for unlimited")
	hash := fs.Bool("hash", false, "issue: store only the sha256 digest, printing the secret once")
	rest, err := parseFlags(fs, args)
	if err != nil {
		return err
	}

	switch sub {
	case "list":
		return listKeys(*path)
	case "issue":
		name := ""
		if len(rest) > 0 {
			name = rest[0]
		}
		return issueKey(*path, name, *admin, *interactive, *hash, *rps)
	case "remove", "revoke":
		if len(rest) == 0 {
			return fmt.Errorf("usage: nanoasr key remove NAME")
		}
		return removeKeys(*path, rest)
	default:
		return fmt.Errorf("key %s: unknown subcommand; use list, issue or remove", sub)
	}
}

func listKeys(path string) error {
	doc, err := openDocument(path)
	if err != nil {
		return err
	}
	keys := doc.Keys()
	if len(keys) == 0 {
		fmt.Printf("no keys in %s\n", path)
		return nil
	}
	fmt.Printf("%-16s %-8s %-10s %-12s %s\n", "NAME", "ADMIN", "RPS", "PRIORITY", "KEY")
	for _, k := range keys {
		fmt.Printf("%-16s %-8v %-10s %-12s %s\n",
			orDash(k.Name), k.Admin, rateOf(k), orDash(priorityOf(k)), maskSecret(k.Key))
	}
	return nil
}

func issueKey(path, name string, admin, interactive, hash bool, rps float64) error {
	if name == "" {
		return fmt.Errorf("usage: nanoasr key issue NAME [-admin] [-rps N] [-interactive] [-hash]")
	}
	doc, err := openDocument(path)
	if err != nil {
		return err
	}

	secret, err := config.NewKeySecret()
	if err != nil {
		return err
	}
	stored := secret
	if hash {
		stored = config.HashSecret(secret)
	}

	k := config.APIKey{Name: name, Key: stored, Admin: admin, RPS: rps}
	if interactive {
		k.Priority = config.PriorityInteractive
	}
	if err := doc.AddKey(k); err != nil {
		return err
	}
	if err := saveDocument(path, doc); err != nil {
		return err
	}

	// The secret goes to stdout and the commentary to stderr, so that
	// `nanoasr key issue ci | ...` pipes the credential and nothing else.
	fmt.Println(secret)
	note := "stored in plaintext"
	if hash {
		note = "stored as a sha256 digest; this is the only time it is printed"
	}
	fmt.Fprintf(os.Stderr, "issued %q in %s (%s)\nrestart the server to load it\n", name, path, note)
	return nil
}

func removeKeys(path string, names []string) error {
	doc, err := openDocument(path)
	if err != nil {
		return err
	}
	var missing []string
	removed := 0
	for _, n := range names {
		if doc.RemoveKey(n) {
			removed++
			continue
		}
		missing = append(missing, n)
	}
	if removed > 0 {
		if err := saveDocument(path, doc); err != nil {
			return err
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("no key named %s in %s", strings.Join(missing, ", "), path)
	}
	fmt.Fprintf(os.Stderr, "removed %d key(s) from %s\nrestart the server to apply it\n", removed, path)
	return nil
}

func openDocument(path string) (*config.Document, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return config.ParseDocument(b)
}

// saveDocument rewrites the file through a temporary one in the same
// directory, so an interrupted write cannot leave a server with no keys at all.
func saveDocument(path string, doc *config.Document) error {
	b, err := doc.Bytes()
	if err != nil {
		return err
	}
	// The rewritten file is parsed back before it replaces anything: an edit
	// that produced something the loader rejects should fail here, while the
	// working file is still the one on disk.
	if _, err := config.ParseDocument(b); err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// configPathDefault matches what serve does, so the two commands act on the
// same file when neither is given a path.
func configPathDefault() string {
	if v := os.Getenv("NANOASR_CONFIG"); v != "" {
		return v
	}
	return "nanoasr.yaml"
}

// maskSecret shows enough of a key to recognise it and not enough to use it.
// A digest is shown whole: it is already not a credential.
func maskSecret(s string) string {
	if strings.HasPrefix(s, "sha256:") {
		return s[:min(len(s), 19)] + "…"
	}
	if len(s) <= 16 {
		return strings.Repeat("*", len(s))
	}
	head := min(len(config.KeyPrefix)+4, len(s)-8)
	return s[:head] + strings.Repeat("*", 8) + s[len(s)-4:]
}

func rateOf(k config.APIKey) string {
	if k.RPS <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%g", k.RPS)
}

func priorityOf(k config.APIKey) string {
	if k.Priority == "" {
		return config.PriorityBatch
	}
	return k.Priority
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
