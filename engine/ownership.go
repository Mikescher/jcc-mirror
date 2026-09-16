package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"blackforestbytes.com/jcc-mirror/store"
)

// makeDir is os.MkdirAll that hands every directory it creates to the pair's
// configured owner and mode. Directories that already exist are left as they are.
func makeDir(pair store.Pair, dir string) error {
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%q exists and is not a directory", dir)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if parent := filepath.Dir(dir); parent != dir {
		if err := makeDir(pair, parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		// Another worker created it first, and that worker settles it.
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	mode, ok := pair.DirModeBits()
	if err := settle(pair, dir, mode, ok); err != nil {
		// Otherwise the next attempt finds it existing and never settles it.
		_ = os.Remove(dir)
		return err
	}
	return nil
}

// settleFile applies the pair's owner and file mode. It has to run before the
// file is renamed into place, so it never appears there with the wrong ones.
func settleFile(pair store.Pair, path string) error {
	mode, ok := pair.FileModeBits()
	return settle(pair, path, mode, ok)
}

// settle changes ownership before the mode: a mode without owner access is only
// workable as root anyway, and root is also what chown needs.
func settle(pair store.Pair, path string, mode fs.FileMode, haveMode bool) error {
	if uid, gid, ok := pair.OwnerIDs(); ok {
		if err := os.Lchown(path, uid, gid); err != nil {
			return fmt.Errorf("hand %q to %d:%d (only root can): %w", path, uid, gid, err)
		}
	}
	if haveMode {
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("set mode %04o on %q: %w", mode, path, err)
		}
	}
	return nil
}
