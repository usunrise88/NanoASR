package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/usunrise88/nanoasr/internal/config"
	"github.com/usunrise88/nanoasr/internal/registry"
)

// pullModels downloads models by id, so a deployment can warm its models
// directory before serving instead of making the first request wait for
// hundreds of megabytes.
func pullModels(cfg config.Config, ids []string) error {
	if len(ids) == 0 {
		return fmt.Errorf("usage: nanoasr models pull <id>...")
	}

	// Pulling is an explicit instruction; refusing it because the config file
	// disables background downloads would be obtuse.
	cfg.Registry.AllowDownload = true

	reg, err := buildRegistry(cfg, quietLogger())
	if err != nil {
		return err
	}
	defer reg.Close()

	ctx := context.Background()
	for _, id := range ids {
		if err := pullOne(ctx, reg, id); err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
	}
	return nil
}

func pullOne(ctx context.Context, reg *registry.Remote, id string) error {
	progress, err := reg.Fetch(ctx, id)
	if err != nil {
		return err
	}

	interactive := isTerminal(os.Stderr)
	shown := -1

	for p := range progress {
		if p.Err != "" {
			fmt.Fprintln(os.Stderr)
			return fmt.Errorf("%s", p.Err)
		}
		if p.Done {
			break
		}
		// Without a terminal, one line per ten percent keeps a CI log readable
		// instead of filling it with carriage returns.
		step := int(p.Percent) / 10
		switch {
		case interactive:
			fmt.Fprintf(os.Stderr, "\r  %-24s %5.1f%% of %s", id, p.Percent, humanBytes(p.Total))
		case step > shown:
			shown = step
			fmt.Fprintf(os.Stderr, "  %-24s %3d%%\n", id, step*10)
		}
	}

	if interactive {
		fmt.Fprintf(os.Stderr, "\r%-60s\r", "")
	}

	dir, err := reg.Dir(id)
	if err != nil {
		return err
	}
	fmt.Printf("%-24s %s\n", id, dir)
	return nil
}

func showCatalog(cfg config.Config) error {
	reg, err := buildRegistry(cfg, quietLogger())
	if err != nil {
		return err
	}
	defer reg.Close()

	entries, err := reg.Catalog(context.Background())
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("the catalog is empty")
		return nil
	}

	installed := map[string]bool{}
	if local, err := reg.Local(context.Background()); err == nil {
		for _, m := range local {
			installed[m.ID] = true
		}
	}

	fmt.Printf("%-24s %-14s %-8s %-8s %-10s %s\n",
		"ID", "FAMILY", "LANGS", "SIZE", "COMMERCIAL", "STATE")
	for _, m := range entries {
		state := "available"
		if installed[m.ID] {
			state = "installed"
		}
		fmt.Printf("%-24s %-14s %-8s %-8s %-10s %s\n",
			m.ID, m.Family, strings.Join(m.Languages, ","),
			humanBytes(m.Source.SizeBytes), orUnknownCommercial(m.CommercialUse), state)
	}
	return nil
}

func orUnknownCommercial(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

func humanBytes(n int64) string {
	switch {
	case n <= 0:
		return "-"
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0fM", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.0fK", float64(n)/(1<<10))
	}
}

// quietLogger keeps registry warnings out of CLI output that is meant to be
// piped; genuine failures still come back as errors.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
