//go:build no_viewer

// Package viewer is left out of a binary built with the tag no_viewer: no
// file is offered to be viewed, and the libraries are not embedded.
package viewer

import "net/http"

// Compiled says the binary was built without the viewer.
const Compiled = false

// Root is the folder below the reserved one the viewer would be served from.
const Root = "/.go-fs/viewer/"

func Available() bool { return false }

func Prefix() string { return "" }

func Page() string { return "" }

func Player() string { return "" }

func Kind(string) string { return "" }

func Editable(string) bool { return false }

func Serve(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
