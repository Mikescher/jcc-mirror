package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func addBackup(t *testing.T, s *Store, pair Pair, at time.Time, reason string) DBBackup {
	t.Helper()
	b := DBBackup{
		PairID: pair.ID, TS: at, RelDB: "ClipCornDB.db",
		Path: "/data/db-backups/" + at.Format("150405"), Size: 4096,
		MTime: at.Add(-time.Hour), Hash: "deadbeef", Reason: reason,
	}
	if err := s.AddDBBackup(context.Background(), &b); err != nil {
		t.Fatalf("AddDBBackup: %v", err)
	}
	return b
}

func TestDBBackupsAreNewestFirst(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	pair := newPair(t, s, "jcc")

	base := time.Now().Truncate(time.Second)
	older := addBackup(t, s, pair, base.Add(-2*time.Hour), BackupReplace)
	newer := addBackup(t, s, pair, base, BackupRollback)

	got, err := s.DBBackups(ctx, pair.ID, 0)
	if err != nil {
		t.Fatalf("DBBackups: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d backups, want 2", len(got))
	}
	if got[0].ID != newer.ID || got[1].ID != older.ID {
		t.Errorf("order is %d, %d; want the newest (%d) first", got[0].ID, got[1].ID, newer.ID)
	}
	if got[0].Reason != BackupRollback || got[0].RelDB != "ClipCornDB.db" {
		t.Errorf("round trip lost fields: %+v", got[0])
	}
	// Whole milliseconds, like every timestamp in this schema.
	if !got[1].MTime.Equal(older.MTime) {
		t.Errorf("mtime = %s, want %s", got[1].MTime, older.MTime)
	}
}

// The keep count is what stops a few MB per sync from becoming a volume
// (DESIGN.md S5). The excess comes back so the caller can remove the files
// before the rows.
func TestExcessDBBackupsLeavesTheNewest(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	pair := newPair(t, s, "jcc")
	other := newPair(t, s, "media")

	base := time.Now().Truncate(time.Second)
	var all []DBBackup
	for i := 0; i < 5; i++ {
		all = append(all, addBackup(t, s, pair, base.Add(time.Duration(i)*time.Minute), BackupReplace))
	}
	addBackup(t, s, other, base, BackupReplace)

	excess, err := s.ExcessDBBackups(ctx, pair.ID, 2)
	if err != nil {
		t.Fatalf("ExcessDBBackups: %v", err)
	}
	if len(excess) != 3 {
		t.Fatalf("got %d excess, want 3", len(excess))
	}
	for _, b := range excess {
		if b.ID == all[4].ID || b.ID == all[3].ID {
			t.Errorf("backup %d is one of the newest two and must be kept", b.ID)
		}
		if err := s.DeleteDBBackup(ctx, b.ID); err != nil {
			t.Fatalf("DeleteDBBackup: %v", err)
		}
	}

	left, err := s.DBBackups(ctx, pair.ID, 0)
	if err != nil {
		t.Fatalf("DBBackups: %v", err)
	}
	if len(left) != 2 {
		t.Errorf("%d backups left, want 2", len(left))
	}
	// The other pair's copies are its own.
	if n := countRows(t, s, "db_backups", other.ID); n != 1 {
		t.Errorf("the other pair has %d backups, want 1", n)
	}
}

func TestDBBackupByIDReportsAMissingOne(t *testing.T) {
	s := newStore(t)
	if _, err := s.DBBackupByID(context.Background(), 404); !errors.Is(err, ErrNoBackup) {
		t.Errorf("DBBackupByID(404) = %v, want ErrNoBackup", err)
	}
}
