package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Scan states. A walk that stopped early is 'interrupted' rather than 'failed',
// because the two are answered differently: an interrupted walk is resumed where
// it left off, a failed one is not resumed at all.
const (
	ScanRunning     = "running"
	ScanInterrupted = "interrupted"
	ScanDone        = "done"
	ScanFailed      = "failed"
)

// Scan is one walk of a pair's remote root.
type Scan struct {
	ID         int64      `json:"id"`
	PairID     int64      `json:"pairId"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Dirs       int64      `json:"dirs"`
	Files      int64      `json:"files"`
	Bytes      int64      `json:"bytes"`
	State      string     `json:"state"`
	Error      string     `json:"error,omitempty"`
}

// ManifestEntry is one row of the last remote walk.
type ManifestEntry struct {
	Path  string    `json:"path"`
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
	IsDir bool      `json:"isDir"`
}

// BeginScan starts a walk. With resume it picks up the one that was interrupted
// instead, which is the whole reason the frontier lives in the manifest rather
// than in the walker's memory: a scan of the real tree runs for minutes and must
// survive a restart (DESIGN.md §2.3).
//
// Any other running scan of the pair is marked failed - two walks writing the
// same manifest would sweep each other's rows.
func (s *Store) BeginScan(ctx context.Context, pairID int64, resume bool) (Scan, bool, error) {
	if resume {
		if sc, ok, err := s.RunningScan(ctx, pairID); err != nil || ok {
			return sc, ok, err
		}
	}

	now := time.Now()
	var sc Scan
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE scans SET state = ?, finished_at = ?, error = ? WHERE pair_id = ? AND state IN (?, ?)`,
			ScanFailed, now.UnixMilli(), "superseded by a new scan", pairID, ScanRunning, ScanInterrupted); err != nil {
			return fmt.Errorf("close previous scan: %w", err)
		}

		res, err := tx.ExecContext(ctx,
			`INSERT INTO scans (pair_id, started_at, state) VALUES (?, ?, ?)`,
			pairID, now.UnixMilli(), ScanRunning)
		if err != nil {
			return fmt.Errorf("start scan: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("start scan: %w", err)
		}

		// The root is the frontier's seed. Rows of the previous walk keep their old
		// scan_id and stay readable until this one finishes and sweeps them, so a
		// diff during a scan still sees the last complete answer.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO manifest (pair_id, relpath, size, mtime, is_dir, listed, scan_id, seen_at)
			 VALUES (?, '', 0, 0, 1, 0, ?, ?)
			 ON CONFLICT (pair_id, relpath) DO UPDATE SET listed = 0, scan_id = excluded.scan_id, seen_at = excluded.seen_at`,
			pairID, id, now.UnixMilli()); err != nil {
			return fmt.Errorf("seed scan frontier: %w", err)
		}

		sc = Scan{ID: id, PairID: pairID, StartedAt: now, State: ScanRunning}
		return nil
	})
	return sc, false, err
}

// RunningScan returns the pair's unfinished walk - the one -resume continues.
func (s *Store) RunningScan(ctx context.Context, pairID int64) (Scan, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+scanColumns+` FROM scans WHERE pair_id = ? AND state IN (?, ?) ORDER BY id DESC LIMIT 1`,
		pairID, ScanRunning, ScanInterrupted)
	sc, err := scanScan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Scan{}, false, nil
	}
	return sc, err == nil, err
}

// PendingDirs returns directories this scan discovered but has not listed yet.
func (s *Store) PendingDirs(ctx context.Context, pairID, scanID int64, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 256
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT relpath FROM manifest
		 WHERE pair_id = ? AND scan_id = ? AND is_dir = 1 AND listed = 0
		 ORDER BY relpath LIMIT ?`, pairID, scanID, limit)
	if err != nil {
		return nil, fmt.Errorf("read scan frontier: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan frontier: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PutListing records one directory's children and marks the directory itself
// listed, in one transaction: a crash between the two would lose the children
// and never look at the directory again.
func (s *Store) PutListing(ctx context.Context, pairID, scanID int64, dir string, entries []ManifestEntry) error {
	now := time.Now().UnixMilli()
	var files, bytes int64

	return s.tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO manifest (pair_id, relpath, size, mtime, is_dir, listed, scan_id, seen_at)
			 VALUES (?, ?, ?, ?, ?, 0, ?, ?)
			 ON CONFLICT (pair_id, relpath) DO UPDATE SET
			     size = excluded.size, mtime = excluded.mtime, is_dir = excluded.is_dir,
			     listed = 0, scan_id = excluded.scan_id, seen_at = excluded.seen_at`)
		if err != nil {
			return fmt.Errorf("prepare manifest insert: %w", err)
		}
		defer stmt.Close()

		for _, e := range entries {
			if _, err := stmt.ExecContext(ctx, pairID, e.Path, e.Size, e.MTime.UnixMilli(), boolInt(e.IsDir), scanID, now); err != nil {
				return fmt.Errorf("write manifest entry %q: %w", e.Path, err)
			}
			if !e.IsDir {
				files++
				bytes += e.Size
			}
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE manifest SET listed = 1, scan_id = ?, seen_at = ? WHERE pair_id = ? AND relpath = ?`,
			scanID, now, pairID, dir); err != nil {
			return fmt.Errorf("mark %q listed: %w", dir, err)
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE scans SET dirs = dirs + 1, files = files + ?, bytes = bytes + ? WHERE id = ?`,
			files, bytes, scanID); err != nil {
			return fmt.Errorf("update scan counters: %w", err)
		}
		return nil
	})
}

// FinishScan closes the walk. Only a completed one sweeps: the rows this scan did
// not see are what the publisher no longer has, and an interrupted walk has not
// seen most of the tree yet. Sweeping on a failure would turn one unreadable
// directory into "everything was deleted".
func (s *Store) FinishScan(ctx context.Context, scanID int64, state, errMsg string) (swept int64, err error) {
	now := time.Now().UnixMilli()

	err = s.tx(ctx, func(tx *sql.Tx) error {
		var pairID int64
		if err := tx.QueryRowContext(ctx, `SELECT pair_id FROM scans WHERE id = ?`, scanID).Scan(&pairID); err != nil {
			return fmt.Errorf("finish scan %d: %w", scanID, err)
		}

		if state == ScanDone {
			res, err := tx.ExecContext(ctx, `DELETE FROM manifest WHERE pair_id = ? AND scan_id <> ?`, pairID, scanID)
			if err != nil {
				return fmt.Errorf("sweep manifest: %w", err)
			}
			swept, _ = res.RowsAffected()
		}

		var msg any
		if errMsg != "" {
			msg = errMsg
		}
		// An interrupted walk keeps finished_at empty: it has not finished, and the
		// next run continues this same row rather than starting another.
		finished := any(now)
		if state == ScanInterrupted {
			finished = nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE scans SET state = ?, finished_at = ?, error = ? WHERE id = ?`,
			state, finished, msg, scanID); err != nil {
			return fmt.Errorf("finish scan %d: %w", scanID, err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return swept, nil
}

const scanColumns = `id, pair_id, started_at, finished_at, dirs, files, bytes, state, error`

// Scans returns a pair's walks, newest first.
func (s *Store) Scans(ctx context.Context, pairID int64, limit int) ([]Scan, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+scanColumns+` FROM scans WHERE pair_id = ? ORDER BY id DESC LIMIT ?`, pairID, limit)
	if err != nil {
		return nil, fmt.Errorf("read scans: %w", err)
	}
	defer rows.Close()

	var out []Scan
	for rows.Next() {
		sc, err := scanScan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// ScanByID returns one walk.
func (s *Store) ScanByID(ctx context.Context, id int64) (Scan, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+scanColumns+` FROM scans WHERE id = ?`, id)
	sc, err := scanScan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Scan{}, fmt.Errorf("no scan with id %d", id)
	}
	return sc, err
}

// LastCompletedScan is what the differ needs: a manifest is only a complete
// answer once a walk has finished.
func (s *Store) LastCompletedScan(ctx context.Context, pairID int64) (Scan, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+scanColumns+` FROM scans WHERE pair_id = ? AND state = ? ORDER BY id DESC LIMIT 1`,
		pairID, ScanDone)
	sc, err := scanScan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Scan{}, false, nil
	}
	return sc, err == nil, err
}

func scanScan(row rowScanner) (Scan, error) {
	var (
		sc       Scan
		started  int64
		finished sql.NullInt64
		errMsg   sql.NullString
	)
	if err := row.Scan(&sc.ID, &sc.PairID, &started, &finished, &sc.Dirs, &sc.Files, &sc.Bytes, &sc.State, &errMsg); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Scan{}, err
		}
		return Scan{}, fmt.Errorf("scan scan row: %w", err)
	}
	sc.StartedAt = time.UnixMilli(started)
	if finished.Valid {
		t := time.UnixMilli(finished.Int64)
		sc.FinishedAt = &t
	}
	sc.Error = errMsg.String
	return sc, nil
}

// ManifestStats totals the last walk, which is how much the publisher has.
func (s *Store) ManifestStats(ctx context.Context, pairID int64) (files, bytes int64, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT count(*), coalesce(sum(size), 0) FROM manifest WHERE pair_id = ? AND is_dir = 0`,
		pairID).Scan(&files, &bytes)
	if err != nil {
		return 0, 0, fmt.Errorf("read manifest stats: %w", err)
	}
	return files, bytes, nil
}

// ManifestEntryAt returns one row of the last walk.
func (s *Store) ManifestEntryAt(ctx context.Context, pairID int64, relpath string) (ManifestEntry, bool, error) {
	var (
		e     ManifestEntry
		ms    int64
		isDir int
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT relpath, size, mtime, is_dir FROM manifest WHERE pair_id = ? AND relpath = ?`,
		pairID, relpath).Scan(&e.Path, &e.Size, &ms, &isDir)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ManifestEntry{}, false, nil
	case err != nil:
		return ManifestEntry{}, false, fmt.Errorf("read manifest entry %q: %w", relpath, err)
	}
	e.MTime, e.IsDir = time.UnixMilli(ms), isDir != 0
	return e, true, nil
}
