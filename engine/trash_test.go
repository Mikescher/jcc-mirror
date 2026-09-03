package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"blackforestbytes.com/jcc-mirror/store"
)

// quarantine writes a file straight into a day's trash directory, which is what
// a deletion N days ago left behind.
func quarantine(t *testing.T, pair store.Pair, day, rel string, size int) {
	t.Helper()

	full := filepath.Join(trashRoot(pair), day, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestTrashCountsWhatEachDayHolds(t *testing.T) {
	pair := store.Pair{Name: "media", LocalPath: t.TempDir()}
	quarantine(t, pair, "2026-09-01", "Filme/a.mkv", 100)
	quarantine(t, pair, "2026-09-01", "Filme/b.mkv", 200)
	quarantine(t, pair, "2026-09-03", "Serien/c.mkv", 300)

	days, err := Trash(pair, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("Trash returned %d days, want 2: %+v", len(days), days)
	}
	// Newest first: the day an operator wants is the one that just happened.
	if days[0].Day != "2026-09-03" || days[0].Files != 1 || days[0].Bytes != 300 {
		t.Errorf("first day = %+v, want 2026-09-03 with one 300-byte file", days[0])
	}
	if days[1].Files != 2 || days[1].Bytes != 300 {
		t.Errorf("second day = %+v, want two files totalling 300 bytes", days[1])
	}
}

// TestPruneTrashOnlyTakesWhatHasExpired is the retention, and the one place in
// jcc-mirror where something is really destroyed.
func TestPruneTrashOnlyTakesWhatHasExpired(t *testing.T) {
	pair := store.Pair{Name: "media", LocalPath: t.TempDir()}
	quarantine(t, pair, "2026-09-01", "old.mkv", 100)
	quarantine(t, pair, "2026-09-09", "new.mkv", 100)

	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.Local)
	removed, err := PruneTrash(pair, 7*24*time.Hour, now)
	if err != nil {
		t.Fatalf("PruneTrash: %v", err)
	}
	if len(removed) != 1 || removed[0].Day != "2026-09-01" {
		t.Fatalf("PruneTrash removed %+v, want only 2026-09-01", removed)
	}

	left, err := Trash(pair, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if len(left) != 1 || left[0].Day != "2026-09-09" {
		t.Errorf("the quarantine holds %+v, want the day that has not expired", left)
	}
}

// TestPruneTrashKeepsEverythingWithoutARetention: an engine with no retention
// configured falls back to seven days, and a zero passed here means "keep".
// Deleting because a setting was empty is not a failure mode this is allowed to
// have.
func TestPruneTrashKeepsEverythingWithoutARetention(t *testing.T) {
	pair := store.Pair{Name: "media", LocalPath: t.TempDir()}
	quarantine(t, pair, "2001-01-01", "ancient.mkv", 100)

	removed, err := PruneTrash(pair, 0, time.Now())
	if err != nil {
		t.Fatalf("PruneTrash: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("PruneTrash with no retention removed %+v, want nothing", removed)
	}
}

// TestTrashDoesNotOverwriteAnEarlierCopy: the publisher removed a file, put it
// back, and removed it again, all in one day.
func TestTrashDoesNotOverwriteAnEarlierCopy(t *testing.T) {
	dir := t.TempDir()
	first, err := trashPath(dir, "Filme/a.mkv")
	if err != nil {
		t.Fatalf("trashPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(first), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(first, []byte("one"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	second, err := trashPath(dir, "Filme/a.mkv")
	if err != nil {
		t.Fatalf("trashPath: %v", err)
	}
	if second == first {
		t.Fatalf("the second copy would land on the first at %s", first)
	}
}

func TestRestoreRefusesToOverwriteWhatIsThere(t *testing.T) {
	pair := store.Pair{Name: "media", LocalPath: t.TempDir()}
	quarantine(t, pair, "2026-09-03", "a.mkv", 10)
	if err := os.WriteFile(filepath.Join(pair.LocalPath, "a.mkv"), []byte("newer"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := RestoreTrash(pair, "", "a.mkv"); err == nil {
		t.Fatal("restore over an existing file succeeded, want a refusal")
	}
	body, err := os.ReadFile(filepath.Join(pair.LocalPath, "a.mkv"))
	if err != nil || string(body) != "newer" {
		t.Errorf("the file in place reads %q (err %v), want it untouched", body, err)
	}
}
