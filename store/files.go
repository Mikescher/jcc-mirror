package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// File states. An adopted row was matched against the remote by size alone
// during the USB bootstrap and has never passed through this process
// (DESIGN.md §2.4).
const (
	FileSynced  = "synced"
	FileAdopted = "adopted"
)

// File is one row of local truth: what lies under the pair's local root, with
// the publisher's mtime rather than the download time.
type File struct {
	PairID     int64     `json:"pairId"`
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	MTime      time.Time `json:"mtime"`
	Hash       string    `json:"hash,omitempty"`
	State      string    `json:"state"`
	VerifiedAt time.Time `json:"verifiedAt"`
}

// PutFile records one file as local truth.
func (s *Store) PutFile(ctx context.Context, f File) error {
	if err := s.tx(ctx, func(tx *sql.Tx) error { return putFileTx(ctx, tx, f) }); err != nil {
		return err
	}
	return nil
}

// PutFiles records many at once. Adoption of the bootstrap writes tens of
// thousands of rows, and one transaction per row would spend the whole run in
// fsync.
func (s *Store) PutFiles(ctx context.Context, files []File) error {
	if len(files) == 0 {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, f := range files {
			if err := putFileTx(ctx, tx, f); err != nil {
				return err
			}
		}
		return nil
	})
}

func putFileTx(ctx context.Context, tx *sql.Tx, f File) error {
	if f.VerifiedAt.IsZero() {
		f.VerifiedAt = time.Now()
	}
	if f.State == "" {
		f.State = FileSynced
	}

	var hash any
	if f.Hash != "" {
		hash = f.Hash
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO files (pair_id, relpath, size, mtime, hash, state, verified_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (pair_id, relpath) DO UPDATE SET
		     size = excluded.size, mtime = excluded.mtime, hash = excluded.hash,
		     state = excluded.state, verified_at = excluded.verified_at`,
		f.PairID, f.Path, f.Size, f.MTime.UnixMilli(), hash, f.State, f.VerifiedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("write file %q: %w", f.Path, err)
	}
	return nil
}

// FileAt returns local truth for one path.
func (s *Store) FileAt(ctx context.Context, pairID int64, relpath string) (File, bool, error) {
	f := File{PairID: pairID}
	var (
		mtime, verified int64
		hash            sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT relpath, size, mtime, hash, state, verified_at FROM files WHERE pair_id = ? AND relpath = ?`,
		pairID, relpath).Scan(&f.Path, &f.Size, &mtime, &hash, &f.State, &verified)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return File{}, false, nil
	case err != nil:
		return File{}, false, fmt.Errorf("read file %q: %w", relpath, err)
	}
	f.MTime, f.VerifiedAt, f.Hash = time.UnixMilli(mtime), time.UnixMilli(verified), hash.String
	return f, true, nil
}

// ForgetFile drops one row of local truth. The file on disk is not touched.
func (s *Store) ForgetFile(ctx context.Context, pairID int64, relpath string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM files WHERE pair_id = ? AND relpath = ?`, pairID, relpath); err != nil {
		return fmt.Errorf("forget file %q: %w", relpath, err)
	}
	return nil
}

// FileStats totals local truth, which is what the dashboard compares against the
// manifest to say how far behind the mirror is.
func (s *Store) FileStats(ctx context.Context, pairID int64) (count, bytes int64, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT count(*), coalesce(sum(size), 0) FROM files WHERE pair_id = ?`, pairID).Scan(&count, &bytes)
	if err != nil {
		return 0, 0, fmt.Errorf("read file stats: %w", err)
	}
	return count, bytes, nil
}

// Change operations.
const (
	ChangeAdd     = "add"
	ChangeReplace = "replace"
	ChangeDelete  = "delete"
	ChangeFail    = "fail"
)

// Change is one line of the per-file history.
type Change struct {
	ID         int64     `json:"id"`
	TS         time.Time `json:"ts"`
	PairID     int64     `json:"pairId"`
	Path       string    `json:"path"`
	Op         string    `json:"op"`
	SizeBefore *int64    `json:"sizeBefore,omitempty"`
	SizeAfter  *int64    `json:"sizeAfter,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// AppendChange records one file's fate. Like the event log it is telemetry: a
// caller logs a failure to write one and carries on with the transfer.
func (s *Store) AppendChange(ctx context.Context, c *Change) error {
	if c.TS.IsZero() {
		c.TS = time.Now()
	}
	var errMsg any
	if c.Error != "" {
		errMsg = c.Error
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO changes (ts, pair_id, relpath, op, size_before, size_after, error) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.TS.UnixMilli(), c.PairID, c.Path, c.Op, c.SizeBefore, c.SizeAfter, errMsg)
	if err != nil {
		return fmt.Errorf("append change %q: %w", c.Path, err)
	}
	c.ID, err = res.LastInsertId()
	if err != nil {
		return fmt.Errorf("append change %q: %w", c.Path, err)
	}
	return nil
}

// ChangeFilter selects a slice of the history. The zero value means "the most
// recent changes, whatever they are".
type ChangeFilter struct {
	PairID *int64
	Ops    []string
	Since  time.Time
	Limit  int

	// Forward and AfterID read oldest first from a row already seen, the way
	// EventFilter does.
	Forward bool
	AfterID int64
}

// Changes returns matching changes, newest first - or oldest first when the
// filter reads forward from an id.
func (s *Store) Changes(ctx context.Context, f ChangeFilter) ([]Change, error) {
	var (
		where []string
		args  []any
	)
	if f.PairID != nil {
		where = append(where, "pair_id = ?")
		args = append(args, *f.PairID)
	}
	if len(f.Ops) > 0 {
		where = append(where, "op IN ("+placeholders(len(f.Ops))+")")
		for _, op := range f.Ops {
			args = append(args, op)
		}
	}
	if !f.Since.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, f.Since.UnixMilli())
	}
	if f.AfterID > 0 {
		where = append(where, "id > ?")
		args = append(args, f.AfterID)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}

	q := `SELECT id, ts, pair_id, relpath, op, size_before, size_after, error FROM changes`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id " + order(f.Forward) + " LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("read changes: %w", err)
	}
	defer rows.Close()

	out := make([]Change, 0, limit)
	for rows.Next() {
		var (
			c             Change
			ms            int64
			pairID        sql.NullInt64
			before, after sql.NullInt64
			errMsg        sql.NullString
		)
		if err := rows.Scan(&c.ID, &ms, &pairID, &c.Path, &c.Op, &before, &after, &errMsg); err != nil {
			return nil, fmt.Errorf("scan change: %w", err)
		}
		c.TS, c.PairID, c.Error = time.UnixMilli(ms), pairID.Int64, errMsg.String
		if before.Valid {
			c.SizeBefore = &before.Int64
		}
		if after.Valid {
			c.SizeAfter = &after.Int64
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PruneChanges drops history older than the retention window (DESIGN.md §6).
func (s *Store) PruneChanges(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM changes WHERE ts < ?`, olderThan.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("prune changes: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune changes: %w", err)
	}
	return n, nil
}
