package registry

import (
	"context"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/usunrise88/nanoasr/internal/core"
)

// RemoteOptions configures a registry that can fetch.
type RemoteOptions struct {
	AllowDownload bool
	// StrictLicense refuses any model that does not positively declare
	// commercial use.
	StrictLicense bool
	// Concurrency caps simultaneous downloads. Two large archives already
	// saturate a typical link; more only makes each slower.
	Concurrency int
	// CatalogYAML overrides the built-in catalog, for a private mirror.
	CatalogYAML []byte
}

// Remote is a Local registry that can also download what the catalog offers.
//
// One download runs per model no matter how many callers ask for it: a
// transcription request that names an absent model and an operator pulling the
// same model must not fetch 160 MB twice. Each caller gets its own progress
// channel, so nobody consumes another's events.
type Remote struct {
	local      *Local
	downloader Downloader
	catalog    []Manifest
	opt        RemoteOptions

	slots chan struct{}

	// baseCtx outlives any single request: a download must not be cancelled
	// because the caller who happened to start it walked away while others
	// are still waiting on it.
	baseCtx context.Context
	cancel  context.CancelFunc

	mu       sync.Mutex
	inflight map[string]*download
}

// NewRemote wraps a local registry with fetching.
func NewRemote(local *Local, downloader Downloader, opt RemoteOptions) (*Remote, error) {
	if opt.Concurrency < 1 {
		opt.Concurrency = 2
	}

	catalog, err := loadCatalog(opt.CatalogYAML)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Remote{
		local:      local,
		downloader: downloader,
		catalog:    catalog,
		opt:        opt,
		slots:      make(chan struct{}, opt.Concurrency),
		baseCtx:    ctx,
		cancel:     cancel,
		inflight:   map[string]*download{},
	}, nil
}

func loadCatalog(override []byte) ([]Manifest, error) {
	if len(override) == 0 {
		return Builtin()
	}
	var f catalogFile
	if err := yaml.Unmarshal(override, &f); err != nil {
		return nil, core.Errorf(core.CodeInvalidRequest, "cannot parse the catalog").WithCause(err)
	}
	return f.Models, nil
}

// Close stops any download still running.
func (r *Remote) Close() error {
	r.cancel()
	return nil
}

// Resolve looks on disk first and falls back to the catalog.
//
// The fallback is what makes automatic download possible at all: without it a
// model that has never been fetched cannot even be named.
func (r *Remote) Resolve(ctx context.Context, id string) (Manifest, error) {
	if m, err := r.local.Resolve(ctx, id); err == nil {
		return m, nil
	}
	if m, ok := r.fromCatalog(id); ok {
		return m, nil
	}
	// Report the local error: it lists what is actually available.
	return r.local.Resolve(ctx, id)
}

func (r *Remote) Local(ctx context.Context) ([]Manifest, error) { return r.local.Local(ctx) }

func (r *Remote) Catalog(context.Context) ([]Manifest, error) {
	return append([]Manifest(nil), r.catalog...), nil
}

func (r *Remote) Dir(id string) (string, error) { return r.local.Dir(id) }

// Ensure blocks until the model is on disk.
func (r *Remote) Ensure(ctx context.Context, id string) (string, error) {
	if dir, err := r.local.Dir(id); err == nil {
		return dir, nil
	}

	dl, err := r.begin(id)
	if err != nil {
		return "", err
	}

	select {
	case <-dl.done:
	case <-ctx.Done():
		// The caller gives up; the download continues for whoever else is
		// waiting on it.
		return "", ctx.Err()
	}
	if dl.err != nil {
		return "", dl.err
	}
	return dl.dir, nil
}

// Fetch starts or joins a download and streams its progress.
func (r *Remote) Fetch(_ context.Context, id string) (<-chan core.DownloadProgress, error) {
	if _, err := r.local.Dir(id); err == nil {
		ch := make(chan core.DownloadProgress, 1)
		ch <- core.DownloadProgress{ModelID: id, Percent: 100, Done: true}
		close(ch)
		return ch, nil
	}

	dl, err := r.begin(id)
	if err != nil {
		return nil, err
	}
	return dl.subscribe(), nil
}

// begin returns the download for id, starting one if none is running.
func (r *Remote) begin(id string) (*download, error) {
	m, ok := r.fromCatalog(id)
	if !ok {
		return nil, core.Errorf(core.CodeModelNotFound,
			"model %q is neither installed nor in the catalog", id)
	}
	if !r.opt.AllowDownload {
		return nil, core.Errorf(core.CodeModelNotFound,
			"model %q is in the catalog but registry.allow_download is off; "+
				"run `nanoasr models pull %s` on a host that can reach the network, "+
				"or place the model in the models directory", id, id)
	}
	if r.opt.StrictLicense && !m.AllowsCommercialUse() {
		return nil, core.Errorf(core.CodeModelForbidden,
			"model %q declares commercial_use=%q and registry.strict_license is on",
			id, orUnknown(m.CommercialUse))
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if dl, running := r.inflight[m.ID]; running {
		return dl, nil
	}

	dl := &download{done: make(chan struct{})}
	r.inflight[m.ID] = dl
	go r.run(m, dl)
	return dl, nil
}

// run performs one download and fans its progress out to every subscriber.
func (r *Remote) run(m Manifest, dl *download) {
	defer func() {
		r.mu.Lock()
		delete(r.inflight, m.ID)
		r.mu.Unlock()

		dl.finish()
	}()

	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-r.baseCtx.Done():
		dl.err = r.baseCtx.Err()
		return
	}

	progress, err := r.downloader.Download(r.baseCtx, m, r.local.root)
	if err != nil {
		dl.err = err
		return
	}

	for p := range progress {
		dl.broadcast(p)
		if p.Done && p.Err != "" {
			dl.err = core.Errorf(core.CodeInternal, "downloading %s: %s", m.ID, p.Err)
		}
	}
	if dl.err != nil {
		return
	}

	// The model is on disk but the registry has not looked since it started.
	if err := r.local.Refresh(r.baseCtx); err != nil {
		dl.err = err
		return
	}
	dir, err := r.local.Dir(m.ID)
	if err != nil {
		dl.err = core.Errorf(core.CodeInternal,
			"model %s downloaded but is not loadable: %v", m.ID, err).WithCause(err)
		return
	}
	dl.dir = dir
}

func (r *Remote) fromCatalog(id string) (Manifest, bool) {
	for _, m := range r.catalog {
		if m.ID == id || m.Key() == id {
			return m, true
		}
	}
	return Manifest{}, false
}

func orUnknown(v string) string {
	if v == "" {
		return CommercialUnknown
	}
	return v
}

// download is one in-flight fetch, shared by every caller that asked for it.
type download struct {
	done chan struct{}
	dir  string
	err  error

	mu          sync.Mutex
	closed      bool
	subscribers []chan core.DownloadProgress
}

// subscribe adds a listener. A caller that joins mid-download sees the rest of
// the updates and the terminal message, which is enough to render progress.
func (d *download) subscribe() <-chan core.DownloadProgress {
	d.mu.Lock()
	defer d.mu.Unlock()

	ch := make(chan core.DownloadProgress, 16)
	if d.closed {
		close(ch)
		return ch
	}
	d.subscribers = append(d.subscribers, ch)
	return ch
}

// broadcast drops an update for a slow subscriber rather than stalling the
// download that produced it.
func (d *download) broadcast(p core.DownloadProgress) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, ch := range d.subscribers {
		select {
		case ch <- p:
		default:
		}
	}
}

func (d *download) finish() {
	d.mu.Lock()
	subs := d.subscribers
	d.subscribers = nil
	d.closed = true
	d.mu.Unlock()

	for _, ch := range subs {
		close(ch)
	}
	close(d.done)
}

var _ Registry = (*Remote)(nil)
