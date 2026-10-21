package registry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

// An application that lets its user choose what to download must not have a
// request fetch a model behind their back.
func TestExplicitOnlyEnsureDoesNotDownload(t *testing.T) {
	d := newFakeDownloader()
	close(d.release)
	r, _ := newRemote(t, d, RemoteOptions{AllowDownload: true, ExplicitOnly: true})

	_, err := r.Ensure(context.Background(), "catalog-model")
	var e *core.Error
	if !errors.As(err, &e) || e.Code != core.CodeModelNotFound {
		t.Fatalf("Ensure of an absent model = %v, want model_not_found", err)
	}
	if got := d.calls.Load(); got != 0 {
		t.Fatalf("Ensure downloaded %d times, want none", got)
	}

	// Asked for by name, it downloads, and Ensure then finds it.
	drain(t, r, "catalog-model")
	if _, err := r.Ensure(context.Background(), "catalog-model"); err != nil {
		t.Fatalf("Ensure after the download: %v", err)
	}
}

func TestRemoveDeletesTheModel(t *testing.T) {
	d := newFakeDownloader()
	close(d.release)
	r, root := newRemote(t, d, RemoteOptions{AllowDownload: true})
	drain(t, r, "catalog-model")

	if err := r.Remove(context.Background(), "catalog-model"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "catalog-model@1")); !os.IsNotExist(err) {
		t.Fatalf("the model directory is still there: %v", err)
	}
	if _, err := r.Dir("catalog-model"); err == nil {
		t.Fatal("the registry still lists the removed model")
	}

	err := r.Remove(context.Background(), "catalog-model")
	var e *core.Error
	if !errors.As(err, &e) || e.Code != core.CodeModelNotFound {
		t.Fatalf("removing it again = %v, want model_not_found", err)
	}
}

func TestCancelStopsADownload(t *testing.T) {
	d := newFakeDownloader()
	r, root := newRemote(t, d, RemoteOptions{AllowDownload: true})

	progress, err := r.Fetch(context.Background(), "catalog-model")
	if err != nil {
		t.Fatal(err)
	}
	<-d.started
	if !r.Downloading("catalog-model") {
		t.Fatal("Downloading is false while the download runs")
	}

	// Removing a model mid-download is refused, not turned into a cancel.
	err = r.Remove(context.Background(), "catalog-model")
	var e *core.Error
	if !errors.As(err, &e) || e.Code != core.CodeModelInUse {
		t.Fatalf("Remove during the download = %v, want model_in_use", err)
	}

	if !r.Cancel("catalog-model@1") {
		t.Fatal("Cancel found no download")
	}
	var last core.DownloadProgress
	for p := range progress {
		last = p
	}
	if !last.Done || last.Err == "" {
		t.Fatalf("last message = %+v, want a failure", last)
	}
	if r.Downloading("catalog-model") {
		t.Fatal("Downloading is still true after the cancel")
	}
	if _, err := os.Stat(filepath.Join(root, "catalog-model@1")); !os.IsNotExist(err) {
		t.Fatalf("a cancelled download left the model behind: %v", err)
	}
	if r.Cancel("catalog-model") {
		t.Fatal("Cancel reported a download that is no longer running")
	}
}

func drain(t *testing.T, r *Remote, id string) {
	t.Helper()
	progress, err := r.Fetch(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case p, ok := <-progress:
			if !ok {
				return
			}
			if p.Err != "" {
				t.Fatalf("download failed: %s", p.Err)
			}
		case <-timeout:
			t.Fatal("the download did not finish")
		}
	}
}
