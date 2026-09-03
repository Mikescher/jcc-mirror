package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Why a copy of the database was taken.
const (
	BackupReplace  = "replace"
	BackupRollback = "rollback"
)

// ErrNoBackup is returned when a rollback names a copy that is not there.
var ErrNoBackup = errors.New("no such database backup")

// DBBackup is one kept copy of a jcc pair's database (DESIGN.md S5).
type DBBackup struct {
	ID     int64     `json:"id"`
	PairID int64     `json:"pairId"`
	TS     time.Time `json:"ts"`
	Path   string    `json:"path"`  // absolute, in the data volume
	RelDB  string    `json:"relDb"` // which database it is a copy of, pair-relative
	Size   int64     `json:"size"`
	MTime  time.Time `json:"mtime"` // the publisher's, carried so a rollback restores it too
	Hash   string    `json:"hash"`
	Reason string    `json:"reason"`
}

const backupColumns = `id, pair_id, ts, relpath, path, size, mtime, hash, reason`

// AddDBBackup records a copy and fills in its ID and timestamp.
func (s *Store) AddDBBackup(ctx context.Context, b *DBBackup) error {
	if b.TS.IsZero() {
		b.TS = time.Now()
	}
	if b.Reason == "" {
		b.Reason = BackupReplace
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO db_backups (pair_id, ts, relpath, path, size, mtime, hash, reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		b.PairID, b.TS.UnixMilli(), b.RelDB, b.Path, b.Size, b.MTime.UnixMilli(), b.Hash, b.Reason)
	if err != nil {
		return fmt.Errorf("record database backup %q: %w", b.Path, err)
	}
	if b.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("record database backup %q: %w", b.Path, err)
	}
	return nil
}

// DBBackups returns a pair's kept copies, newest first - which is the order both
// the CLI and the dashboard number them in.
func (s *Store) DBBackups(ctx context.Context, pairID int64, limit int) ([]DBBackup, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+backupColumns+` FROM db_backups WHERE pair_id = ? ORDER BY ts DESC, id DESC LIMIT ?`,
		pairID, limit)
	if err != nil {
		return nil, fmt.Errorf("read database backups: %w", err)
	}
	defer rows.Close()

	var out []DBBackup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DBBackupByID returns one copy.
func (s *Store) DBBackupByID(ctx context.Context, id int64) (DBBackup, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+backupColumns+` FROM db_backups WHERE id = ?`, id)
	b, err := scanBackup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return DBBackup{}, fmt.Errorf("%w with id %d", ErrNoBackup, id)
	}
	return b, err
}

// ExcessDBBackups returns the copies past the newest keep, oldest first. The
// caller removes the files and then the rows: a row without its file is a
// rollback that fails at the last moment, and a file without its row is one
// nobody can find (DESIGN.md S5).
func (s *Store) ExcessDBBackups(ctx context.Context, pairID int64, keep int) ([]DBBackup, error) {
	if keep < 1 {
		keep = 1
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+backupColumns+` FROM db_backups
		 WHERE pair_id = ? ORDER BY ts DESC, id DESC LIMIT -1 OFFSET ?`, pairID, keep)
	if err != nil {
		return nil, fmt.Errorf("read database backups: %w", err)
	}
	defer rows.Close()

	var out []DBBackup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteDBBackup drops one row. The file it names is the caller's to remove.
func (s *Store) DeleteDBBackup(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM db_backups WHERE id = ?`, id); err != nil {
		return fmt.Errorf("forget database backup %d: %w", id, err)
	}
	return nil
}

func scanBackup(row rowScanner) (DBBackup, error) {
	var (
		b         DBBackup
		ts, mtime int64
	)
	if err := row.Scan(&b.ID, &b.PairID, &ts, &b.RelDB, &b.Path, &b.Size, &mtime, &b.Hash, &b.Reason); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DBBackup{}, err
		}
		return DBBackup{}, fmt.Errorf("scan database backup: %w", err)
	}
	b.TS, b.MTime = time.UnixMilli(ts), time.UnixMilli(mtime)
	return b, nil
}
