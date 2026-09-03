// Package store is jcc-mirror's own state: the configuration, the event log, the
// pairs, the manifest of the publisher's tree, local truth, the job queue and the
// transfer history. It is a sqlite database in the data volume, opened with the
// pure-Go driver so the binary stays static and cross-compiles to the Synology
// without a libc to match.
//
// WAL is right for this database - unlike ClipCorn's, it has no attached second
// file to stay atomic with (DESIGN.md §7).
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DBName is the file the store lives in, inside the data directory.
const DBName = "jcc-mirror.db"

type Store struct {
	db *sql.DB
}

// Open opens, and if necessary creates, the store in dataDir.
func Open(ctx context.Context, dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data directory %q: %w", dataDir, err)
	}
	return OpenFile(ctx, filepath.Join(dataDir, DBName))
}

// OpenFile opens the store at an explicit path. ":memory:" works and is what the
// tests use.
func OpenFile(ctx context.Context, path string) (*Store, error) {
	// foreign_keys is off by default in sqlite and has to be asked for per
	// connection; busy_timeout turns "database is locked" from an error the caller
	// has to handle into a wait, which is what it should be for a single process
	// with a handful of goroutines.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Ping reports whether the database still answers. It is what /healthz checks:
// the volume going away underneath a running container is a real failure mode on
// a NAS, and everything else the process reports would still look fine.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// DB exposes the handle for the packages that own their own tables.
func (s *Store) DB() *sql.DB { return s.db }

// tx runs fn in a transaction, rolling back on any error or panic.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
