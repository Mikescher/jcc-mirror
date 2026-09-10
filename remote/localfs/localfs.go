// Package localfs serves a local directory as a remote.Remote. It is the fake the
// design asks for: the scanner, differ and transfer engine can be driven with no
// network, no tunnel and no NAS (DESIGN.md §2.1).
//
// Its timestamps are deliberately the coarsest granularity anywhere in the
// pipeline - whole seconds - so a test that passes here is not passing because it
// got a precision the manifest could never have stored.
package localfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	"blackforestbytes.com/jcc-mirror/remote"
)

type FS struct {
	root string
}

var (
	_ remote.RangeReader = (*FS)(nil)
	_ remote.Prober      = (*FS)(nil)
)

// New serves root, which must be an existing directory.
func New(root string) (*FS, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", root, err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("open remote root: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("remote root %q is not a directory", abs)
	}
	return &FS{root: abs}, nil
}

// Root returns the served directory, for diagnostics.
func (f *FS) Root() string { return f.root }

// List implements remote.Remote.
func (f *FS) List(ctx context.Context, dir string) ([]remote.Entry, error) {
	rel, full, err := f.resolve(dir)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	names, err := os.ReadDir(full)
	if err != nil {
		return nil, fmt.Errorf("list %q: %w", dir, err)
	}

	out := make([]remote.Entry, 0, len(names))
	for _, d := range names {
		child := path.Join(rel, norm.NFC.String(d.Name()))
		// Stat rather than d.Info: the publisher's server resolves symlinks, and an
		// entry whose target is gone is simply not listed rather than failing the
		// whole directory.
		st, err := os.Stat(filepath.Join(full, d.Name()))
		if err != nil {
			continue
		}
		out = append(out, entryOf(child, st))
	}

	// os.ReadDir sorts by the on-disk name; sorting by the normalized path is what
	// the SMB client does too, so the two agree on order.
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Stat implements remote.Remote.
func (f *FS) Stat(ctx context.Context, p string) (remote.Entry, error) {
	rel, full, err := f.resolve(p)
	if err != nil {
		return remote.Entry{}, err
	}
	if err := ctx.Err(); err != nil {
		return remote.Entry{}, err
	}

	st, err := os.Stat(full)
	if err != nil {
		return remote.Entry{}, fmt.Errorf("stat %q: %w", p, err)
	}
	return entryOf(rel, st), nil
}

// Exists implements remote.Prober, which is what the jcc pair's lock gate probes
// with. A directory counts as present: the gate asks about one named file, and
// answering "no" for something that is there would be the wrong kind of wrong.
func (f *FS) Exists(ctx context.Context, p string) (bool, error) {
	_, full, err := f.resolve(p)
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	switch _, err := os.Lstat(full); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("stat %q: %w", p, err)
	}
}

// Open implements remote.Remote.
func (f *FS) Open(ctx context.Context, p string, offset int64) (io.ReadCloser, error) {
	rc, _, err := f.OpenRange(ctx, p, offset, 0)
	return rc, err
}

// OpenRange implements remote.RangeReader, which is what the transfer engine
// actually uses. Without it here the engine would be exercised through a
// different code path in every test than the one that runs against DSM.
func (f *FS) OpenRange(ctx context.Context, p string, offset, length int64) (io.ReadCloser, int64, error) {
	_, full, err := f.resolve(p)
	if err != nil {
		return nil, 0, err
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if offset < 0 {
		return nil, 0, fmt.Errorf("open %q: negative offset %d", p, offset)
	}

	fh, err := os.Open(full)
	if err != nil {
		return nil, 0, fmt.Errorf("open %q: %w", p, err)
	}

	st, err := fh.Stat()
	if err != nil {
		fh.Close()
		return nil, 0, fmt.Errorf("open %q: %w", p, err)
	}
	// A range that starts past the end of the file is an error on the real remote,
	// not an empty body, and the transfer engine has to see the same thing here.
	if offset > st.Size() {
		fh.Close()
		return nil, 0, fmt.Errorf("open %q: offset %d is past the end (%d bytes)", p, offset, st.Size())
	}
	if _, err := fh.Seek(offset, io.SeekStart); err != nil {
		fh.Close()
		return nil, 0, fmt.Errorf("open %q at %d: %w", p, offset, err)
	}
	if length <= 0 {
		return fh, st.Size(), nil
	}
	return sectionReader{Reader: io.LimitReader(fh, length), Closer: fh}, st.Size(), nil
}

// sectionReader bounds a read without losing the ability to close the file.
type sectionReader struct {
	io.Reader
	io.Closer
}

// resolve turns a remote-relative path into its normalized form and its location
// on disk, refusing anything that would leave the root.
func (f *FS) resolve(rel string) (string, string, error) {
	clean := strings.Trim(rel, "/")
	if clean != "" {
		clean = strings.TrimPrefix(path.Clean("/"+clean), "/")
	}
	clean = norm.NFC.String(clean)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", fmt.Errorf("path %q lies outside the remote root", rel)
	}

	full := f.root
	if clean != "" {
		full = filepath.Join(f.root, filepath.FromSlash(clean))
	}
	return clean, full, nil
}

func entryOf(rel string, st os.FileInfo) remote.Entry {
	e := remote.Entry{
		Path:  rel,
		Name:  path.Base("/" + rel),
		IsDir: st.IsDir(),
		// Truncated on purpose, to something coarser than the pipeline carries
		// anywhere: the manifest stores milliseconds, so a test must not be able to
		// pass on a precision that could never have survived a real mirror.
		MTime: st.ModTime().UTC().Truncate(time.Second),
	}
	if rel == "" {
		e.Name = ""
	}
	if !e.IsDir {
		e.Size = st.Size()
	}
	return e
}
