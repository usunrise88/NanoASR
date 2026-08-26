//go:build noui

package ui

import "net/http"

// Enabled reports whether this build contains a usable UI. A var rather than a
// const so that both builds expose the same kind of symbol.
var Enabled = false

// Handler always fails in a noui build; the server logs and skips mounting.
func Handler(string) (http.Handler, error) {
	return nil, errNoUI{}
}

type errNoUI struct{}

func (errNoUI) Error() string { return "this binary was built with -tags noui" }
