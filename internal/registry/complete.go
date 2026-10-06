package registry

import (
	"context"
	"os"
	"path/filepath"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Complete fetches the files a catalog entry names beside its archive that an
// installed model is missing, and returns the ones it added.
//
// It exists because of how a model arrives twice. The first time, the archive
// and everything named beside it are downloaded together and the manifest is
// written out complete. But a model installed by an older build predates the
// entry naming those files, and nothing about it is wrong enough to re-download
// a hundred and seventy megabytes: the weights are the same weights, and what
// is missing is a vocabulary of a quarter of a megabyte.
//
// So this is the repair, and it is an explicit instruction rather than
// something a server does to itself at startup. Reaching the network because a
// catalog entry changed is a surprise on a machine that was working; `nanoasr
// models pull` is where somebody has already decided to download something.
//
// The manifest on disk is edited rather than replaced. It may carry an
// operator's own changes — the documentation told people to add exactly this
// file by hand before the catalog could — and rewriting it from the catalog
// would silently discard them.
func (r *Remote) Complete(ctx context.Context, id string) ([]string, error) {
	dir, err := r.local.Dir(id)
	if err != nil {
		// Not installed: there is nothing to complete, and Ensure is what
		// installs it.
		return nil, nil
	}
	catalog, ok := r.fromCatalog(id)
	if !ok {
		// A model that is only on disk is whatever its own manifest says. The
		// catalog has no opinion to apply.
		return nil, nil
	}

	var missing []ExtraFile
	for _, extra := range catalog.Source.Extra {
		if _, err := os.Stat(filepath.Join(dir, extra.Name)); err != nil {
			missing = append(missing, extra)
		}
	}

	local, err := ReadManifest(filepath.Join(dir, ManifestFile))
	if err != nil {
		return nil, err
	}
	// Roles the catalog knows about and the installed manifest does not: the
	// file may already be on disk, put there by hand, and still be invisible
	// because nothing points at it.
	added := map[string]string{}
	for role, name := range catalog.Files {
		if local.Files[role] == "" {
			added[role] = name
		}
	}
	if len(missing) == 0 && len(added) == 0 {
		return nil, nil
	}

	if len(missing) > 0 {
		if !r.opt.AllowDownload {
			return nil, core.Errorf(core.CodeModelNotFound,
				"model %s is missing %s and registry.allow_download is off",
				id, names(missing))
		}
		// Into the model directory through a temporary name, so an interrupted
		// fetch cannot leave a half-written vocabulary that loads.
		for _, extra := range missing {
			tmp, err := os.CreateTemp(dir, "."+extra.Name+".*")
			if err != nil {
				return nil, core.Errorf(core.CodeInternal,
					"cannot write into %s", dir).WithCause(err)
			}
			path := tmp.Name()
			_ = tmp.Close()

			if err := r.downloader.FetchExtra(ctx, catalog, extra, path); err != nil {
				_ = os.Remove(path)
				return nil, err
			}
			if err := os.Rename(path, filepath.Join(dir, extra.Name)); err != nil {
				_ = os.Remove(path)
				return nil, core.Errorf(core.CodeInternal,
					"cannot install %s", extra.Name).WithCause(err)
			}
		}
	}

	if len(added) > 0 {
		if local.Files == nil {
			local.Files = map[string]string{}
		}
		for role, name := range added {
			local.Files[role] = name
		}
		if err := writeManifest(filepath.Join(dir, ManifestFile), local); err != nil {
			return nil, err
		}
	}

	// The scan holds the manifest it read at startup; without this the model
	// keeps answering from the one that had no vocabulary.
	if err := r.local.Refresh(ctx); err != nil {
		return nil, err
	}

	out := make([]string, 0, len(missing)+len(added))
	for _, extra := range missing {
		out = append(out, extra.Name)
	}
	for role := range added {
		if !containsName(out, local.Files[role]) {
			out = append(out, local.Files[role])
		}
	}
	return out, nil
}

func names(extras []ExtraFile) string {
	out := ""
	for i, e := range extras {
		if i > 0 {
			out += ", "
		}
		out += e.Name
	}
	return out
}

func containsName(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
