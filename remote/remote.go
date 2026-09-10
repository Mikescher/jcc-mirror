// Package remote is the publisher side of the mirror, reduced to the three
// operations the engine actually needs. Everything else in jcc-mirror talks to
// this interface, so the scanner, differ and transfer engine can be driven by a
// local fake with no network and no share to mount (DESIGN.md §2.1).
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
	MTime time.Time `json:"mtime"` // as the remote reports it; the manifest keeps milliseconds, see DESIGN.md §2.3
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

	// Open streams path starting at offset. An implementation that cannot honour a
	// non-zero offset must report an error, never hand back the file from the
	// start - otherwise a resume writes the start of the file into the middle of
	// it. Every implementation owes the resume logic that guarantee.
	Open(ctx context.Context, path string, offset int64) (io.ReadCloser, error)
}

// Prober is a Remote that can answer "is this one file there" without listing the
// directory around it. The lock gate of DESIGN.md §3 is what needs it: the lock
// sits beside a database that may share a directory with ten thousand covers, and
// an existence check is all the gate ever wants - the body of a .~lock is a PID
// from another machine and is deliberately never read.
//
// Like RangeReader it is kept out of Remote, so the three-method interface stays
// what a fake has to implement.
type Prober interface {
	Remote

	// Exists reports whether path is present. A path that is not there is false
	// and no error; anything else - a refused request, a tunnel that is down - is
	// an error, because the gate must never read "cannot tell" as "nobody is
	// using it".
	Exists(ctx context.Context, path string) (bool, error)
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
