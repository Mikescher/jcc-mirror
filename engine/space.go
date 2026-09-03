package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/store"
)

// ErrNoStatfs is what FreeSpace returns where the platform cannot answer. The
// preflight treats it as "unknown" and lets the transfer run: refusing to mirror
// because the free space could not be read would be the worse failure.
var ErrNoStatfs = errors.New("free space cannot be read on this platform")

// Space is what a volume has left.
type Space struct {
	Path  string `json:"path"`
	Free  int64  `json:"free"` // available to us, which is not the same as unused
	Total int64  `json:"total"`
}

// FreeSpace reports the volume holding path. A path that does not exist yet -
// a pair whose local root has never been written to - is answered for its
// nearest existing parent, which is on the same volume the directory will be
// created on.
func FreeSpace(path string) (Space, error) {
	dir := filepath.Clean(path)
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return Space{Path: path}, fmt.Errorf("no existing directory above %q", path)
		}
		dir = parent
	}

	free, total, err := statfs(dir)
	if err != nil {
		return Space{Path: dir}, err
	}
	return Space{Path: dir, Free: free, Total: total}, nil
}

// SpaceError is a preflight that says no. It is a type rather than a message
// because the transfer engine has to tell it apart from a file that failed: no
// number of retries makes a full volume smaller, so the run stops rather than
// grinding the whole queue through its attempts (DESIGN.md §2.6, S4).
type SpaceError struct {
	Path    string
	Need    int64
	Free    int64
	Reserve int64
}

func (e *SpaceError) Error() string {
	return fmt.Sprintf("not enough room under %s: %s to transfer, %s free, %s reserved; narrow the pair with excludes or lower the reserve",
		e.Path, format.Bytes(e.Need), format.Bytes(e.Free), format.Bytes(e.Reserve))
}

// checkSpace refuses a transfer whose completion would eat into the reserve. It
// is called for the whole plan before a run starts and again for each file,
// since a plan can run for days and something else on the NAS may fill the
// volume while it does.
func (e *Engine) checkSpace(pair store.Pair, need int64) error {
	if need <= 0 {
		return nil
	}

	space, err := FreeSpace(pair.LocalPath)
	if err != nil {
		if !errors.Is(err, ErrNoStatfs) {
			e.log.Warnf("space: %v", err)
		}
		return nil
	}
	if space.Free-e.opts.Reserve >= need {
		return nil
	}
	return &SpaceError{Path: space.Path, Need: need, Free: space.Free, Reserve: e.opts.Reserve}
}
