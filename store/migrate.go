package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrate applies every migration the binary carries that the file has not seen.
// The version lives in sqlite's own user_version, so there is no bootstrap table
// and no chicken-and-egg on a fresh file.
func (s *Store) migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	var current int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("database is at schema version %d but this binary only knows %d - it was written by a newer jcc-mirror", current, len(migrations))
	}

	for _, m := range migrations[current:] {
		if err := s.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

type migration struct {
	version int
	name    string
	sql     string
}

// noForeignKeys marks a migration that rebuilds a referenced table. Such a
// migration has to run with foreign keys off, or dropping the old table cascades
// into everything that references it.
const noForeignKeys = "-- foreign_keys: off\n"

func (s *Store) applyMigration(ctx context.Context, m migration) error {
	if strings.HasPrefix(m.sql, noForeignKeys) {
		return s.applyWithoutForeignKeys(ctx, m)
	}
	return s.tx(ctx, func(tx *sql.Tx) error { return runMigration(ctx, tx, m) })
}

// applyWithoutForeignKeys pins one connection: the pragma is per connection, and
// sqlite ignores it inside a transaction.
func (s *Store) applyWithoutForeignKeys(ctx context.Context, m migration) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	// The connection goes back to the pool, and every other one has them on.
	defer conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys = ON`)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration %s: begin transaction: %w", m.name, err)
	}
	defer tx.Rollback()

	if err := runMigration(ctx, tx, m); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("migration %s: check foreign keys: %w", m.name, err)
	}
	broken := rows.Next()
	rows.Close()
	if broken {
		return fmt.Errorf("migration %s: leaves rows referencing something that is not there", m.name)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %s: commit: %w", m.name, err)
	}
	return nil
}

func runMigration(ctx context.Context, tx *sql.Tx, m migration) error {
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	// PRAGMA user_version takes no placeholder, and the value is an int parsed
	// from the filename, so there is nothing to inject.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, m.version)); err != nil {
		return fmt.Errorf("migration %s: set version: %w", m.name, err)
	}
	return nil
}

// loadMigrations reads the embedded migrations, which must be named
// NNNN_name.sql and numbered from 1 without gaps.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		num, _, ok := strings.Cut(name, "_")
		if !ok {
			return nil, fmt.Errorf("migration %q: expected NNNN_name.sql", name)
		}
		version, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", name, err)
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", name, err)
		}
		out = append(out, migration{version: version, name: name, sql: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %q is numbered %d, expected %d", m.name, m.version, i+1)
		}
	}
	return out, nil
}
