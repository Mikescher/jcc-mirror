// Package remote is the publisher side of the mirror, reduced to the three
// operations the engine actually needs. Everything else in jcc-mirror talks to
// this interface, so the scanner, differ and transfer engine can be driven by a
// local fake with no network and no WebDAV server (DESIGN.md §2.1).
package remote

import (
	"context"
	"io"
	"time"
)

// Entry is one node of a remote tree.
type Entry struct {
	Path  string    `json:"path"`  // relative to the remote root, slash-separated, no leading slash, NFC-normalized
	Name  string    `json:"name"`  // last path segment
	Size  int64     `json:"size"`  // 0 for directories
	MTime time.Time `json:"mtime"` // one-second granularity over WebDAV, see DESIGN.md §2.3
	IsDir bool      `json:"isDir"`
}

// Remote is a read-only view of the publisher's share. There is deliberately no
// write side: the replication is one-way and nothing of ours runs over there.
type Remote interface {
	// List returns the direct children of dir. dir is a remote-root-relative
	// path; "" is the root itself. The returned entries never include dir.
	List(ctx context.Context, dir string) ([]Entry, error)

	// Stat returns the entry for a single path.
	Stat(ctx context.Context, path string) (Entry, error)

	// Open streams path starting at offset. A non-zero offset that the server
	// silently ignores must be reported as an error, never as a short read -
	// otherwise a resume writes the start of the file into the middle of it.
	Open(ctx context.Context, path string, offset int64) (io.ReadCloser, error)
}

// RangeReader is a Remote that can serve a bounded range. The transfer engine
// requires it: without a length, parallel streams within one file would each run
// to the end of it, and the whole file would come down once per stream. It is
// kept out of Remote itself so the three-method interface stays the thing a
// diagnostic or a fake has to implement.
//
// length <= 0 means "to the end". The second return value is the file's total
// size, or -1 where the server did not say.
type RangeReader interface {
	Remote
	OpenRange(ctx context.Context, path string, offset, length int64) (io.ReadCloser, int64, error)
}
