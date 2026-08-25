package main

import (
	_ "embed"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/usunrise88/nanoasr/internal/config"
)

//go:embed init.yaml
var initTemplate string

// Models a fresh installation needs to answer a Russian request end to end.
//
// They are named here rather than taken from config.Default() because these are
// downloads, and a default that silently changes which two gigabytes an
// operator fetches would be worse than one they can read in the source.
const (
	initASRModel = "gigaam-v3-ctc-punct-ru"
	initVADModel = "silero-vad-v5"
	initSegModel = "pyannote-segmentation-3"
	initEmbModel = "campplus-sv-voxceleb"
)

// initCommand writes a working configuration and fetches the weights it names.
//
// It exists because the shortest honest answer to "how do I run this" was five
// steps long: write a config, invent two secrets, work out which four models a
// Russian deployment needs, pull them, and only then start the server. Every
// one of those is mechanical, and the two that are not — where the data lives
// and what the secrets are — are exactly what this prints at the end.
func initCommand(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	cfgPath := fs.String("config", "nanoasr.yaml", "configuration file to write")
	dataDir := fs.String("data-dir", "", "where models and the database live (default: /var/lib/nanoasr if writable, else ./nanoasr-data)")
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	model := fs.String("model", initASRModel, "default recognition model")
	noDiarize := fs.Bool("no-diarize", false, "skip the two speaker models")
	noDownload := fs.Bool("no-download", false, "write the configuration but download nothing")
	force := fs.Bool("force", false, "overwrite an existing configuration file")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}

	if _, err := os.Stat(*cfgPath); err == nil && !*force {
		return fmt.Errorf("%s already exists; pass -force to overwrite it, "+
			"or use `nanoasr key issue` to add a key to the one you have", *cfgPath)
	}

	dir, err := resolveDataDir(*dataDir)
	if err != nil {
		return err
	}

	adminKey, err := config.NewKeySecret()
	if err != nil {
		return err
	}
	userKey, err := config.NewKeySecret()
	if err != nil {
		return err
	}

	host, _ := os.Hostname()
	if host == "" {
		host = "this machine"
	}

	rendered, err := renderInit(map[string]string{
		"Host":      host,
		"Addr":      *addr,
		"AdminKey":  adminKey,
		"UserKey":   userKey,
		"ModelsDir": filepath.Join(dir, "models"),
		"DBPath":    filepath.Join(dir, "nanoasr.db"),
		"ASRModel":  *model,
		"VADModel":  initVADModel,
		"SegModel":  quoteIfEmpty(pick(!*noDiarize, initSegModel)),
		"EmbModel":  quoteIfEmpty(pick(!*noDiarize, initEmbModel)),
		"Diarize":   strconv.FormatBool(!*noDiarize),
		"Threshold": defaultThreshold(),
	})
	if err != nil {
		return err
	}

	// Written only after the loader has accepted it, so `nanoasr init` cannot
	// leave behind a file that `nanoasr serve` refuses.
	if err := writeConfig(*cfgPath, rendered); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("the generated configuration did not load: %w", err)
	}

	fmt.Printf("configuration  %s\n", *cfgPath)
	fmt.Printf("data           %s\n", dir)

	if *noDownload {
		fmt.Fprintln(os.Stderr, "\nno models were downloaded (-no-download); run "+
			"`nanoasr models pull "+strings.Join(initModels(*model, !*noDiarize), " ")+"` before serving")
	} else if err := pullModels(cfg, initModels(*model, !*noDiarize)); err != nil {
		// The configuration is already on disk and valid, so a download that
		// failed is worth reporting as itself rather than as "init failed":
		// the fix is `nanoasr models pull`, not starting over.
		return fmt.Errorf("the configuration was written, but downloading the models failed: %w", err)
	}

	fmt.Printf("\nadmin key      %s\n", adminKey)
	fmt.Printf("user key       %s\n", userKey)
	fmt.Fprintf(os.Stderr, "\nBoth keys are stored in %s. Start the server with:\n"+
		"  nanoasr serve -config %s\n", *cfgPath, *cfgPath)
	return nil
}

func initModels(asr string, diarize bool) []string {
	ids := []string{asr, initVADModel}
	if diarize {
		ids = append(ids, initSegModel, initEmbModel)
	}
	return ids
}

func renderInit(vars map[string]string) ([]byte, error) {
	t, err := template.New("init").Option("missingkey=error").Parse(initTemplate)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	if err := t.Execute(&b, vars); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// writeConfig writes the file at 0600. It holds API keys in plaintext, so it is
// no more readable than a private key would be.
func writeConfig(path string, b []byte) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, b, 0o600)
}

// resolveDataDir picks where models and the database live.
//
// /var/lib/nanoasr is where the systemd unit expects them, and it is the right
// answer whenever this is running as the service user. When it is not — a
// developer trying the binary in a checkout — failing with a permission error
// would be a poor greeting, so the working directory is used instead and the
// choice is printed rather than assumed.
func resolveDataDir(explicit string) (string, error) {
	if explicit != "" {
		if err := os.MkdirAll(explicit, 0o755); err != nil {
			return "", err
		}
		return filepath.Abs(explicit)
	}
	const system = "/var/lib/nanoasr"
	if err := os.MkdirAll(system, 0o755); err == nil && writable(system) {
		return system, nil
	}
	local := "nanoasr-data"
	if err := os.MkdirAll(local, 0o755); err != nil {
		return "", err
	}
	return filepath.Abs(local)
}

func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

func pick(cond bool, v string) string {
	if cond {
		return v
	}
	return ""
}

// quoteIfEmpty keeps an unset model from rendering as a bare empty scalar,
// which YAML reads as null and the loader then reports as a type error rather
// than as the empty string it means.
func quoteIfEmpty(v string) string {
	if v == "" {
		return `""`
	}
	return v
}

func defaultThreshold() string {
	return strconv.FormatFloat(float64(config.Default().Diarization.Clustering.Threshold), 'g', -1, 32)
}
