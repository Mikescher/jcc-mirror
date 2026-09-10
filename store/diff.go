package store

import (
	"context"
	"fmt"
	"time"
)

// DiffRow is one file the last remote walk and local truth disagree about.
type DiffRow struct {
	Path      string
	Size      int64     // on the publisher
	MTime     time.Time // on the publisher
	Have      bool      // a local row exists, so this is a replace rather than an add
	LocalSize int64
}

// ChangedFiles streams every remote file that is new here or differs from what
// we hold, oldest path first. It is a join rather than two full table reads: the
// manifest of the real collection is six figures of rows, and the differ has to
// stay flat in memory.
//
// The mtime comparison carries a tolerance because the manifest stores mtimes as
// milliseconds while the local timestamp keeps whatever granularity its
// filesystem has. Comparing for equality would make every file look changed on
// every scan, forever (DESIGN.md §2.3).
func (s *Store) ChangedFiles(ctx context.Context, pairID int64, tolerance time.Duration, fn func(DiffRow) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.relpath, m.size, m.mtime, f.relpath IS NOT NULL, coalesce(f.size, 0)
		 FROM manifest m
		 LEFT JOIN files f ON f.pair_id = m.pair_id AND f.relpath = m.relpath
		 WHERE m.pair_id = ? AND m.is_dir = 0
		   AND (f.relpath IS NULL OR f.size <> m.size OR abs(f.mtime - m.mtime) > ?)
		 ORDER BY m.relpath`,
		pairID, tolerance.Milliseconds())
	if err != nil {
		return fmt.Errorf("diff pair %d: %w", pairID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			d  DiffRow
			ms int64
		)
		if err := rows.Scan(&d.Path, &d.Size, &ms, &d.Have, &d.LocalSize); err != nil {
			return fmt.Errorf("scan diff row: %w", err)
		}
		d.MTime = time.UnixMilli(ms)
		if err := fn(d); err != nil {
			return err
		}
	}
	return rows.Err()
}

// VanishedFiles streams the files we hold that the last walk did not find. It is
// the read-only half: a plan counts them and reports them, and only the pass that
// acts on them needs VanishedPage (DESIGN.md §2.5).
func (s *Store) VanishedFiles(ctx context.Context, pairID int64, fn func(path string, size int64) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT f.relpath, f.size
		 FROM files f
		 LEFT JOIN manifest m ON m.pair_id = f.pair_id AND m.relpath = f.relpath AND m.is_dir = 0
		 WHERE f.pair_id = ? AND m.relpath IS NULL
		 ORDER BY f.relpath`, pairID)
	if err != nil {
		return fmt.Errorf("read vanished files of pair %d: %w", pairID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			path string
			size int64
		)
		if err := rows.Scan(&path, &size); err != nil {
			return fmt.Errorf("scan vanished file: %w", err)
		}
		if err := fn(path, size); err != nil {
			return err
		}
	}
	return rows.Err()
}

// VanishedFile is one row of that list.
type VanishedFile struct {
	Path string
	Size int64
}

// VanishedPage returns the same list one page at a time, in path order, starting
// after the path the last page ended on.
//
// The deletion phase pages rather than streams because it deletes the rows it is
// reading: a cursor left open over a table being written under it is not a thing
// to build a deletion on. The cursor is a path rather than an offset, so the rows
// that went do not shift the rows that are left.
func (s *Store) VanishedPage(ctx context.Context, pairID int64, after string, limit int) ([]VanishedFile, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT f.relpath, f.size
		 FROM files f
		 LEFT JOIN manifest m ON m.pair_id = f.pair_id AND m.relpath = f.relpath AND m.is_dir = 0
		 WHERE f.pair_id = ? AND m.relpath IS NULL AND f.relpath > ?
		 ORDER BY f.relpath LIMIT ?`, pairID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read vanished files of pair %d: %w", pairID, err)
	}
	defer rows.Close()

	out := make([]VanishedFile, 0, limit)
	for rows.Next() {
		var v VanishedFile
		if err := rows.Scan(&v.Path, &v.Size); err != nil {
			return nil, fmt.Errorf("scan vanished file: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// RemoteFiles streams the last walk itself, which is what adoption matches the
// local tree against.
func (s *Store) RemoteFiles(ctx context.Context, pairID int64, fn func(ManifestEntry) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT relpath, size, mtime FROM manifest WHERE pair_id = ? AND is_dir = 0 ORDER BY relpath`, pairID)
	if err != nil {
		return fmt.Errorf("read manifest of pair %d: %w", pairID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			e  ManifestEntry
			ms int64
		)
		if err := rows.Scan(&e.Path, &e.Size, &ms); err != nil {
			return fmt.Errorf("scan manifest row: %w", err)
		}
		e.MTime = time.UnixMilli(ms)
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}
