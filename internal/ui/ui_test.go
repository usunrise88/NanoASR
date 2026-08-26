//go:build !noui

package ui

import (
	"testing"
	"testing/fstest"
)

// dist always contains something: a committed placeholder page keeps
// `go build ./cmd/nanoasr` working in a fresh checkout. Telling the two apart
// is what stops a binary built without `make web` from reporting a UI it does
// not have and serving an apology at 200.
func TestHasSPA(t *testing.T) {
	for _, tc := range []struct {
		name string
		fsys fstest.MapFS
		want bool
	}{
		{
			name: "a real build",
			fsys: fstest.MapFS{
				"dist/index.html":               {Data: []byte("<html>")},
				"dist/assets/index-BCZu9dNZ.js": {Data: []byte("//")},
			},
			want: true,
		},
		{
			name: "the committed placeholder",
			fsys: fstest.MapFS{"dist/index.html": {Data: []byte("<html>")}},
			want: false,
		},
		{
			name: "an assets directory with nothing in it",
			fsys: fstest.MapFS{"dist/index.html": {Data: []byte("<html>")},
				"dist/assets/.keep": {Data: []byte("")}},
			want: true,
		},
		{
			name: "no dist at all",
			fsys: fstest.MapFS{},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasSPA(tc.fsys); got != tc.want {
				t.Errorf("hasSPA = %v, want %v", got, tc.want)
			}
		})
	}
}

// A build with only the placeholder must refuse to mount, and say what to do.
func TestHandlerRefusesAPlaceholderBuild(t *testing.T) {
	if Enabled {
		t.Skip("this binary carries a real SPA; the refusal path needs a placeholder build")
	}
	_, err := Handler("/ui")
	if err == nil {
		t.Fatal("Handler succeeded on a placeholder-only build")
	}
	if err != errNoSPA {
		t.Errorf("err = %v, want errNoSPA", err)
	}
}
