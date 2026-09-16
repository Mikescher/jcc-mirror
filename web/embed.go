// Package web is the built Angular dashboard, compiled into the binary so the
// deployment stays one file and one volume (DESIGN.md §4).
//
// dist/ is not checked in and must be built (`make web`) before this package
// compiles; every Go target in the Makefile does that first.
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
