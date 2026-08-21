package registry

import (
	"context"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Registry resolves model ids to on-disk manifests, and fetches what is missing.
type Registry interface {
	// Resolve returns the manifest for id, consulting local models first and
	// the catalog second.
	Resolve(ctx context.Context, id string) (Manifest, error)
	// Local lists models present on disk.
	Local(ctx context.Context) ([]Manifest, error)
	// Catalog lists models available for download.
	Catalog(ctx context.Context) ([]Manifest, error)
	// Ensure makes the model present locally, downloading if allowed, and
	// returns its directory. Progress is nil when nothing needs downloading.
	Ensure(ctx context.Context, id string) (dir string, progress <-chan core.DownloadProgress, err error)
	// Dir returns the directory of an already-present model.
	Dir(id string) (string, error)
}

// Downloader fetches and unpacks a model archive.
//
// Requirements that are not optional (SPEC §7.3, §11): verify sha256 against
// the catalog before unpacking; unpack into a temp directory and os.Rename into
// place so a partial download is never visible; refuse archive entries with
// "..", absolute paths or symlinks; cap total unpacked size and file count.
type Downloader interface {
	Download(ctx context.Context, m Manifest, destDir string) (<-chan core.DownloadProgress, error)
}
