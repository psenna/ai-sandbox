// Package webui embeds the static frontend (docker-operator/web) using Go's
// embed directive and serves it, so the shipped binary needs no sidecar
// files.
//
// The embed directive cannot reach outside its own package directory, and it
// refuses to follow a symlinked directory (verified empirically: "cannot
// embed irregular file"), so this package embeds internal/webui/web -- a
// COMMITTED COPY of docker-operator/web, kept in sync the same way
// internal/agent/dind-init.sh tracks scripts/dind-init.sh: `make
// sync-web-embed` regenerates the copy from the real source, and `make
// web-embed-check` (part of `make all`) fails if the two have drifted. Test
// files (*.test.js) are deliberately excluded from the copy -- nothing at
// runtime needs to serve them.
//
// If internal/webui/web is ever deleted without regenerating it, go:embed's
// own compile error ("pattern web: no matching files found") fails `go
// build` immediately, so a missing copy can never silently ship an empty UI.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web
var assets embed.FS

// Handler returns an http.Handler serving the embedded frontend rooted at
// "/" -- index.html at "/", style.css at "/style.css", vendor/xterm/xterm.js
// at "/vendor/xterm/xterm.js", and so on. Register it on a mux alongside
// more specific patterns (e.g. "/api/", "/ws/"); net/http's ServeMux prefers
// the most specific matching pattern, so those routes take precedence over
// this catch-all without any ordering requirement.
//
// The error return exists only so callers can fail startup with a clear
// message instead of panicking; it is non-nil only if the embedded
// filesystem is malformed in a way a successful `go build` already rules
// out (fs.Sub fails solely when "web" is not a valid embedded path).
func Handler() (http.Handler, error) {
	sub, err := fs.Sub(assets, "web")
	if err != nil {
		return nil, err
	}
	return http.FileServer(http.FS(sub)), nil
}
