package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"blackforestbytes.com/jcc-mirror/store"
)

// TrashDir is where a deleted file goes instead of away. It sits under PartDir,
// which is under the pair's local root, so the move is a rename on the same
// filesystem rather than a copy of a 40 GB file - and so a deletion of the whole
// collection costs no space at all until the retention runs out (DESIGN.md §2.5).
const TrashDir = "trash"

// dayLayout names a day's directory. Dating the quarantine rather than the file
// is what makes the retention sweep a directory removal instead of a walk of
// everything ever deleted.
const dayLayout = "2006-01-02"

// TrashDay is one day's worth of quarantined files.
type TrashDay struct {
	Day     string    `json:"day"`
	Path    string    `json:"path"`
	Files   int       `json:"files"`
	Bytes   int64     `json:"bytes"`
	Expires time.Time `json:"expires"`
}

// Expired reports whether the retention has run out on this day's files.
func (d TrashDay) Expired(now time.Time) bool { return !d.Expires.IsZero() && now.After(d.Expires) }

// trashRoot is where a pair's quarantine lives.
func trashRoot(pair store.Pair) string {
	return filepath.Join(pair.LocalPath, PartDir, TrashDir)
}

// trashDirFor is the day's directory inside a pair's quarantine.
func trashDirFor(pair store.Pair, at time.Time) string {
	return filepath.Join(trashRoot(pair), at.Format(dayLayout))
}

// trashPath is where a file goes when it is deleted, keeping its relative path
// under the day's directory so a restore is the same move backwards.
//
// The suffix is for the file deleted twice in one day - the publisher removed it,
// put it back, and removed it again - which is rare and must still not overwrite
// the first copy: quarantine that loses a file is not quarantine.
func trashPath(dir, relpath string) (string, error) {
	base := filepath.Join(dir, filepath.FromSlash(relpath))
	for i := 0; i < 100; i++ {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s.%d", base, i)
		}
		_, err := os.Lstat(candidate)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return candidate, nil
		case err != nil:
			return "", fmt.Errorf("check the quarantine path for %q: %w", relpath, err)
		}
	}
	return "", fmt.Errorf("%q is already quarantined a hundred times over", relpath)
}

// Trash reports what a pair holds in quarantine, newest day first. retention is
// what the Expires field is computed against; zero leaves it unset.
func Trash(pair store.Pair, retention time.Duration) ([]TrashDay, error) {
	root := trashRoot(pair)
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the quarantine of %q: %w", pair.Name, err)
	}

	var out []TrashDay
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		day, err := time.ParseInLocation(dayLayout, e.Name(), time.Local)
		if err != nil {
			// Not ours. Anything else under the quarantine root is left alone rather
			// than swept up: the sweep removes whole directories.
			continue
		}

		d := TrashDay{Day: e.Name(), Path: filepath.Join(root, e.Name())}
		if retention > 0 {
			d.Expires = day.Add(retention)
		}
		if err := filepath.WalkDir(d.Path, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			d.Files++
			d.Bytes += info.Size()
			return nil
		}); err != nil {
			return nil, fmt.Errorf("read the quarantine of %q: %w", pair.Name, err)
		}
		out = append(out, d)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Day > out[j].Day })
	return out, nil
}

// PruneTrash removes the days whose retention has run out, and returns them.
// This is the only place in jcc-mirror where data is destroyed rather than moved,
// which is why it is one function with one caller-visible result.
func PruneTrash(pair store.Pair, retention time.Duration, now time.Time) ([]TrashDay, error) {
	if retention <= 0 {
		return nil, nil
	}

	days, err := Trash(pair, retention)
	if err != nil {
		return nil, err
	}

	var removed []TrashDay
	for _, d := range days {
		if !d.Expired(now) {
			continue
		}
		if err := os.RemoveAll(d.Path); err != nil {
			return removed, fmt.Errorf("empty the quarantine of %s: %w", d.Day, err)
		}
		removed = append(removed, d)
	}
	return removed, nil
}

// Restored is one file put back.
type Restored struct {
	Path string `json:"path"` // relative to the pair's local root
	From string `json:"from"` // where in the quarantine it came from
	Size int64  `json:"size"`
}

// RestoreTrash moves a quarantined file back where it was. day may be empty, in
// which case the newest copy is the one restored.
//
// It deliberately writes no row of local truth: a restored file is one the
// publisher does not have, so leaving it unrecorded is what keeps the next mirror
// run from quarantining it a second time. It is a file the operator now owns.
func RestoreTrash(pair store.Pair, day, relpath string) (Restored, error) {
	relpath = strings.Trim(strings.TrimSpace(relpath), "/")
	if relpath == "" {
		return Restored{}, errors.New("no path to restore")
	}

	days, err := Trash(pair, 0)
	if err != nil {
		return Restored{}, err
	}

	for _, d := range days {
		if day != "" && d.Day != day {
			continue
		}
		src := filepath.Join(d.Path, filepath.FromSlash(relpath))
		st, err := os.Stat(src)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Restored{}, fmt.Errorf("read %q from the quarantine: %w", relpath, err)
		}

		dst := localPath(pair, relpath)
		if _, err := os.Lstat(dst); err == nil {
			return Restored{}, fmt.Errorf("%q is already back in place; nothing was moved", dst)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return Restored{}, fmt.Errorf("create %q: %w", filepath.Dir(dst), err)
		}
		if err := os.Rename(src, dst); err != nil {
			return Restored{}, fmt.Errorf("restore %q: %w", relpath, err)
		}
		return Restored{Path: relpath, From: src, Size: st.Size()}, nil
	}

	where := "the quarantine"
	if day != "" {
		where = "the quarantine of " + day
	}
	return Restored{}, fmt.Errorf("%q is not in %s", relpath, where)
}

// pruneEmptyDirs removes the directories a deletion left behind, from the file's
// own upwards, and stops at the first one that still holds something. It never
// climbs past the pair's local root and never touches PartDir, which holds the
// quarantine the deletion just wrote into.
func pruneEmptyDirs(pair store.Pair, relpath string) {
	root := filepath.Clean(pair.LocalPath)
	part := filepath.Join(root, PartDir)

	dir := filepath.Dir(localPath(pair, relpath))
	for dir != root && strings.HasPrefix(dir, root+string(filepath.Separator)) && dir != part {
		// Remove refuses a directory that is not empty, which is exactly the test
		// wanted - and doing it without a separate read is what keeps this cheap
		// when a deletion of ten thousand files walks the same parents.
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
