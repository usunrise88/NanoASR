package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/usunrise88/nanoasr/internal/core"
)

// UnpackLimits bound what an archive is allowed to become.
//
// The archive is fetched over the network and its manifest may come from a
// mirror, so the numbers exist to make a hostile archive fail rather than fill
// the disk. They are generous: the largest catalog entry unpacks to about
// 300 MB across a dozen files.
type UnpackLimits struct {
	MaxArchiveBytes  int64
	MaxUnpackedBytes int64
	MaxFileBytes     int64
	MaxFiles         int
}

// DefaultUnpackLimits leaves roughly an order of magnitude of headroom over
// the largest model we ship.
func DefaultUnpackLimits() UnpackLimits {
	return UnpackLimits{
		MaxArchiveBytes:  4 << 30,
		MaxUnpackedBytes: 8 << 30,
		MaxFileBytes:     4 << 30,
		MaxFiles:         512,
	}
}

// DownloadOptions configures the downloader.
type DownloadOptions struct {
	// Mirrors are base URLs tried in order when the manifest URL fails. The
	// archive file name is appended to each.
	Mirrors []string
	Timeout time.Duration
	Retries int
	Limits  UnpackLimits
	Client  *http.Client
}

// HTTPDownloader fetches and unpacks a model archive.
type HTTPDownloader struct {
	client  *http.Client
	mirrors []string
	retries int
	limits  UnpackLimits
}

func NewHTTPDownloader(opt DownloadOptions) *HTTPDownloader {
	if opt.Timeout <= 0 {
		opt.Timeout = 30 * time.Minute
	}
	if opt.Retries <= 0 {
		opt.Retries = 3
	}
	if opt.Limits == (UnpackLimits{}) {
		opt.Limits = DefaultUnpackLimits()
	}

	client := opt.Client
	if client == nil {
		client = &http.Client{Timeout: opt.Timeout}
	}
	// Redirects are allowed to change host — GitHub releases redirect to their
	// object storage — but never to drop to plaintext. Integrity does not rest
	// on the host list: it rests on the checksum, which no redirect can forge.
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		if req.URL.Scheme != "https" {
			return fmt.Errorf("refusing redirect to %s: downloads must stay on https", req.URL.Scheme)
		}
		return nil
	}

	return &HTTPDownloader{
		client:  client,
		mirrors: opt.Mirrors,
		retries: opt.Retries,
		limits:  opt.Limits,
	}
}

// Download fetches the model described by m and installs it into destDir.
//
// The returned channel carries progress and is always closed. A failure is
// reported on the channel's last message and returned by the caller's Ensure;
// nothing partially installed is ever left where the registry can see it.
func (d *HTTPDownloader) Download(ctx context.Context, m Manifest, destDir string) (<-chan core.DownloadProgress, error) {
	if m.Source.URL == "" {
		return nil, core.Errorf(core.CodeModelNotFound,
			"model %s has no download URL", m.ID)
	}
	if m.Source.SHA256 == "" {
		return nil, core.Errorf(core.CodeInvalidRequest,
			"model %s has no sha256; refusing to install an unverifiable download", m.ID)
	}
	if err := requireHTTPS(m.Source.URL); err != nil {
		return nil, err
	}

	progress := make(chan core.DownloadProgress, 8)
	go func() {
		defer close(progress)
		if err := d.run(ctx, m, destDir, progress); err != nil {
			send(progress, core.DownloadProgress{ModelID: m.ID, Done: true, Err: err.Error()})
			return
		}
		send(progress, core.DownloadProgress{
			ModelID: m.ID, Total: m.Source.SizeBytes,
			Downloaded: m.Source.SizeBytes, Percent: 100, Done: true,
		})
	}()
	return progress, nil
}

func (d *HTTPDownloader) run(ctx context.Context, m Manifest, destDir string, progress chan<- core.DownloadProgress) error {
	// Everything happens beside the destination and moves into place at the
	// end, so an interrupted download never leaves a directory the registry
	// would happily load a broken model from.
	work, err := os.MkdirTemp(destDir, ".download-"+m.ID+"-*")
	if err != nil {
		return core.Errorf(core.CodeInternal, "cannot create a work directory").WithCause(err)
	}
	defer os.RemoveAll(work)

	archive := filepath.Join(work, archiveName(m.Source.URL))
	if err := d.fetch(ctx, m, archive, progress); err != nil {
		return err
	}

	unpacked := filepath.Join(work, "unpacked")
	if err := Unpack(archive, unpacked, d.limits); err != nil {
		return err
	}
	// Release the archive before the rename: on a small disk, holding 160 MB
	// we no longer need while writing the model out is avoidable.
	_ = os.Remove(archive)

	if err := flattenSingleDirectory(unpacked); err != nil {
		return err
	}

	target := filepath.Join(destDir, m.Key())
	if err := os.RemoveAll(target); err != nil {
		return core.Errorf(core.CodeInternal, "cannot replace %s", target).WithCause(err)
	}
	if err := os.Rename(unpacked, target); err != nil {
		return core.Errorf(core.CodeInternal, "cannot install into %s", target).WithCause(err)
	}

	// The manifest travels with the model: a directory the catalog described
	// must be self-describing once it is on disk, or a restart forgets what it
	// is.
	return writeManifest(filepath.Join(target, ManifestFile), m)
}

// fetch downloads the archive, resuming a partial file when one is present,
// and verifies the checksum before anyone unpacks anything.
func (d *HTTPDownloader) fetch(ctx context.Context, m Manifest, dest string, progress chan<- core.DownloadProgress) error {
	urls := append([]string{m.Source.URL}, mirrorURLs(d.mirrors, m.Source.URL)...)

	var lastErr error
	for _, raw := range urls {
		for attempt := range d.retries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if attempt > 0 {
				// A short backoff: the common failure is a dropped connection
				// mid-transfer, and the resume picks up where it stopped.
				select {
				case <-time.After(time.Duration(1<<attempt) * time.Second):
				case <-ctx.Done():
					return ctx.Err()
				}
			}

			err := d.attempt(ctx, m, raw, dest, progress)
			if err == nil {
				return nil
			}
			lastErr = err
			if isNonRetryable(err) {
				// This source will not improve on a second attempt, but
				// another one might: a missing artefact or corrupted bytes on
				// the primary host is the reason mirrors exist.
				break
			}
		}
	}
	// The reason travels in the message, not only in the wrapped cause: an
	// operator reading "could not download" without the checksum or the HTTP
	// status has nothing to act on.
	return core.Errorf(core.CodeInternal,
		"could not download model %s from any source: %v", m.ID, lastErr).WithCause(lastErr)
}

func (d *HTTPDownloader) attempt(ctx context.Context, m Manifest, rawURL, dest string, progress chan<- core.DownloadProgress) error {
	if err := requireHTTPS(rawURL); err != nil {
		return err
	}

	hasher := sha256.New()
	offset, err := resumeOffset(dest, hasher)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// The server ignored the range: start over rather than append to a
		// prefix that is about to be sent again.
		if offset > 0 {
			hasher.Reset()
			offset = 0
		}
	case http.StatusPartialContent:
	case http.StatusNotFound, http.StatusForbidden, http.StatusGone:
		return permanentf("model archive is not available at %s (%s)", rawURL, resp.Status)
	default:
		return fmt.Errorf("unexpected status %s from %s", resp.Status, rawURL)
	}

	total := m.Source.SizeBytes
	if total <= 0 && resp.ContentLength > 0 {
		total = offset + resp.ContentLength
	}
	if total > d.limits.MaxArchiveBytes {
		return permanentf("archive is %d bytes, over the %d byte limit", total, d.limits.MaxArchiveBytes)
	}

	flags := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(dest, flags, 0o600)
	if err != nil {
		return err
	}

	written, err := copyWithProgress(ctx, io.MultiWriter(f, hasher), resp.Body,
		m.ID, offset, total, d.limits.MaxArchiveBytes, progress)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(got, m.Source.SHA256) {
		// A mismatch means the bytes on disk are worthless; keeping them would
		// make the next attempt resume onto a corrupt prefix.
		_ = os.Remove(dest)
		return permanentf("checksum mismatch for %s: got %s, expected %s", m.ID, got, m.Source.SHA256)
	}
	_ = written
	return nil
}

// resumeOffset hashes any bytes already on disk so a resumed download still
// produces the checksum of the whole file.
func resumeOffset(dest string, hasher io.Writer) (int64, error) {
	f, err := os.Open(dest)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	n, err := io.Copy(hasher, f)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// copyWithProgress streams the body, enforcing the size cap as it goes and
// reporting no more often than four times a second.
func copyWithProgress(ctx context.Context, dst io.Writer, src io.Reader, modelID string, offset, total, limit int64, progress chan<- core.DownloadProgress) (int64, error) {
	buf := make([]byte, 256<<10)
	done := offset
	last := time.Time{}

	for {
		if err := ctx.Err(); err != nil {
			return done, err
		}

		n, err := src.Read(buf)
		if n > 0 {
			if done+int64(n) > limit {
				return done, permanentf("archive exceeds the %d byte limit", limit)
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return done, werr
			}
			done += int64(n)

			if time.Since(last) > 250*time.Millisecond {
				last = time.Now()
				send(progress, core.DownloadProgress{
					ModelID: modelID, Downloaded: done, Total: total,
					Percent: percent(done, total),
				})
			}
		}
		if err != nil {
			if err == io.EOF {
				return done, nil
			}
			return done, err
		}
	}
}

// flattenSingleDirectory lifts the contents of a lone top-level directory.
//
// Every sherpa-onnx archive wraps its files in a directory named after the
// release, and keeping that nesting would put the weights one level below
// where the manifest says they are.
func flattenSingleDirectory(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return nil
	}

	inner := filepath.Join(root, entries[0].Name())
	nested, err := os.ReadDir(inner)
	if err != nil {
		return err
	}
	for _, e := range nested {
		if err := os.Rename(filepath.Join(inner, e.Name()), filepath.Join(root, e.Name())); err != nil {
			return err
		}
	}
	return os.Remove(inner)
}

// writeManifest leaves the catalog entry beside the weights, so a directory
// the catalog described is self-describing once installed and a restart does
// not have to consult the catalog to know what it is holding.
func writeManifest(path string, m Manifest) error {
	body, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	header := "# Written by nanoasr when this model was downloaded.\n" +
		"# Edits are lost if the model is re-downloaded.\n"
	return os.WriteFile(path, append([]byte(header), body...), 0o644)
}

func mirrorURLs(mirrors []string, primary string) []string {
	name := archiveName(primary)
	out := make([]string, 0, len(mirrors))
	for _, base := range mirrors {
		out = append(out, strings.TrimSuffix(base, "/")+"/"+name)
	}
	return out
}

func archiveName(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		if base := path.Base(u.Path); base != "" && base != "/" && base != "." {
			return base
		}
	}
	return "model.archive"
}

func requireHTTPS(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return core.Errorf(core.CodeInvalidRequest, "malformed download URL %q", rawURL)
	}
	if u.Scheme != "https" {
		return core.Errorf(core.CodeInvalidRequest,
			"download URL %q must use https, got %q", rawURL, u.Scheme)
	}
	return nil
}

func percent(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(done) / float64(total) * 100
}

// send drops an update rather than block: a slow consumer must not wedge the
// download that is feeding it, and a missed percentage costs nothing.
func send(ch chan<- core.DownloadProgress, p core.DownloadProgress) {
	if p.Done {
		// The terminal message carries the outcome, including the error, so it
		// is worth waiting for — but not forever, since a consumer that walked
		// away would otherwise leak this goroutine.
		select {
		case ch <- p:
		case <-time.After(5 * time.Second):
		}
		return
	}
	select {
	case ch <- p:
	default:
	}
}

// permanentError marks a failure that retrying the same source cannot fix.
// The next source is still worth trying.
type permanentError struct{ error }

func permanentf(format string, args ...any) error {
	return permanentError{fmt.Errorf(format, args...)}
}

func isNonRetryable(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}
