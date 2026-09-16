package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	// A file rather than :memory:, because database/sql pools connections and each
	// connection to :memory: would get a database of its own.
	s, err := OpenFile(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	s, err := OpenFile(ctx, path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := s.ConfigSet(ctx, map[string]string{KeyJCCDBDir: "ro"}, "test"); err != nil {
		t.Fatalf("ConfigSet: %v", err)
	}
	s.Close()

	s, err = OpenFile(ctx, path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s.Close()

	got, err := s.ConfigGet(ctx, KeyJCCDBDir)
	if err != nil {
		t.Fatalf("ConfigGet: %v", err)
	}
	if got != "ro" {
		t.Errorf("value after reopen = %q, want %q", got, "ro")
	}
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	s, err := OpenFile(ctx, path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `PRAGMA user_version = 999`); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	s.Close()

	// Downgrading onto a database a newer binary wrote has to stop, not migrate:
	// the data volume outlives any single binary in this design.
	if _, err := OpenFile(ctx, path); err == nil {
		t.Fatal("opening a newer schema succeeded, want an error")
	}
}

func TestLoadMigrationsAreNumberedWithoutGaps(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("no migrations embedded")
	}
	for i, m := range ms {
		if m.version != i+1 {
			t.Errorf("migration %q has version %d, want %d", m.name, m.version, i+1)
		}
	}
}

// TestSMBMigrationLeavesATrail: the WebDAV base URL is dropped rather than kept,
// and it named the two things an operator now has to enter again - the
// publisher's address and the directory the mirror was rooted at. Losing it in
// silence would leave nothing to copy them from.
//
// The migration has already run on a fresh file, so the row it removes is put
// back and the statements are replayed over it.
func TestSMBMigrationLeavesATrail(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	const stale = "http://10.13.13.2:5005/media"
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO config (key, value, updated_at) VALUES ('remote.url', ?, 0)`, stale); err != nil {
		t.Fatalf("plant the stale row: %v", err)
	}

	body, err := migrationFS.ReadFile("migrations/0007_smb_remote.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("apply the migration: %v", err)
	}

	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM config WHERE key = 'remote.url'`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Errorf("the stale key survived the migration")
	}

	events, err := s.Events(ctx, EventFilter{Kinds: []string{KindConfigChanged}})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 || !strings.Contains(events[0].Message, stale) {
		t.Fatalf("the old URL was dropped without a trail: %+v", events)
	}
}

// TestUpdateHTTPMigration: update.remote goes, and so does an update.url that is
// a path on the share, while an http(s) one stays.
func TestUpdateHTTPMigration(t *testing.T) {
	ctx := context.Background()

	body, err := migrationFS.ReadFile("migrations/0010_update_http.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}

	for url, keep := range map[string]bool{
		"dist/jcc-mirror-amd64":        false,
		"  HTTPS://cloud.example/bin":  true,
		"http://10.13.13.2/jcc-mirror": true,
	} {
		s := newStore(t)
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO config (key, value, updated_at) VALUES ('update.remote', 'publisher', 0), ('update.url', ?, 0)`, url); err != nil {
			t.Fatalf("plant the rows: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("apply the migration: %v", err)
		}

		var remotes, urls int
		if err := s.db.QueryRowContext(ctx,
			`SELECT count(*) FILTER (WHERE key = 'update.remote'), count(*) FILTER (WHERE key = 'update.url') FROM config`).
			Scan(&remotes, &urls); err != nil {
			t.Fatalf("count: %v", err)
		}
		if remotes != 0 {
			t.Errorf("%q: update.remote survived", url)
		}
		if (urls == 1) != keep {
			t.Errorf("%q: %d update.url rows left, want kept = %v", url, urls, keep)
		}
	}
}

// TestPairModesMigration runs 0012 over a database stopped at 0011: a mirror pair
// becomes a guarded one, and what references it survives the table rebuild.
func TestPairModesMigration(t *testing.T) {
	ctx := context.Background()
	s := &Store{}
	var err error
	s.db, err = sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "test.db")+"?_txlock=immediate&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	ms, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range ms[:11] {
		if err := s.applyMigration(ctx, m); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO pairs (id, name, type, remote_path, local_path, mode, created_at, updated_at, owner)
		VALUES (1, 'movies', 'raw', 'Filme', '/mnt/Filme', 'mirror', 0, 0, '1026:100'),
		       (2, 'series', 'raw', 'Serien', '/mnt/Serien', 'additive', 0, 0, '');
		INSERT INTO files (pair_id, relpath, size, mtime, state, verified_at) VALUES (1, 'a.mkv', 1, 0, 'synced', 0);
		INSERT INTO changes (ts, pair_id, relpath, op) VALUES (0, 1, 'a.mkv', 'add');`); err != nil {
		t.Fatalf("plant the rows: %v", err)
	}

	if err := s.applyMigration(ctx, ms[11]); err != nil {
		t.Fatalf("apply %s: %v", ms[11].name, err)
	}

	pairs, err := s.Pairs(ctx)
	if err != nil {
		t.Fatalf("Pairs: %v", err)
	}
	modes := map[string]string{}
	for _, p := range pairs {
		modes[p.Name] = p.Mode
	}
	if modes["movies"] != ModeGuarded || modes["series"] != ModeAdditive {
		t.Errorf("modes after the migration = %v, want movies guarded and series additive", modes)
	}
	if pairs[0].Owner != "1026:100" {
		t.Errorf("owner = %q, want it carried over", pairs[0].Owner)
	}

	var files, changes int
	if err := s.db.QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM files WHERE pair_id = 1), (SELECT count(*) FROM changes WHERE pair_id = 1)`).
		Scan(&files, &changes); err != nil {
		t.Fatalf("count: %v", err)
	}
	if files != 1 || changes != 1 {
		t.Errorf("the rebuild took %d files and %d changes with it, want 1 and 1", 1-files, 1-changes)
	}

	if _, err := s.db.ExecContext(ctx, `DELETE FROM pairs WHERE id = 1`); err != nil {
		t.Fatalf("delete pair: %v", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM files`).Scan(&files); err != nil {
		t.Fatalf("count: %v", err)
	}
	if files != 0 {
		t.Error("foreign keys stayed off after the migration: deleting the pair left its files")
	}
}
