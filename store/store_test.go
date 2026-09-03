package store

import (
	"context"
	"path/filepath"
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
	if _, err := s.ConfigSet(ctx, map[string]string{KeyRemoteUser: "ro"}, "test"); err != nil {
		t.Fatalf("ConfigSet: %v", err)
	}
	s.Close()

	s, err = OpenFile(ctx, path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s.Close()

	got, err := s.ConfigGet(ctx, KeyRemoteUser)
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
