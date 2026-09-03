// Package web is the built Angular dashboard, compiled into the binary so the
// deployment stays one file and one volume (DESIGN.md §4).
//
// dist/ is checked in on purpose: `go build` then needs no node toolchain, which
// is what keeps `make syno` and the Dockerfile as simple as they were. Rebuild it
// with `make web` after changing anything under src/.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS is the dashboard's files, rooted at index.html.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // the embed directive above guarantees the directory exists
	}
	return sub
}
