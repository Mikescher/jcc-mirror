package store

import (
	"context"
	"slices"
	"testing"
	"time"
)

// manifestTime is a fixed instant with millisecond precision, which is what the
// store columns hold.
var manifestTime = time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC)

// putManifest records a whole walk in one step: one scan whose root listing is
// entries, finished successfully.
func putManifest(t *testing.T, s *Store, pairID int64, entries ...ManifestEntry) {
	t.Helper()
	ctx := context.Background()

	sc, _, err := s.BeginScan(ctx, pairID, false)
	if err != nil {
		t.Fatalf("BeginScan: %v", err)
	}
	if err := s.PutListing(ctx, pairID, sc.ID, "", entries); err != nil {
		t.Fatalf("PutListing: %v", err)
	}
	if _, err := s.FinishScan(ctx, sc.ID, ScanDone, ""); err != nil {
		t.Fatalf("FinishScan: %v", err)
	}
}

func pendingDirs(t *testing.T, s *Store, pairID, scanID int64) []string {
	t.Helper()
	dirs, err := s.PendingDirs(context.Background(), pairID, scanID, 0)
	if err != nil {
		t.Fatalf("PendingDirs: %v", err)
	}
	return dirs
}

func TestBeginScanSeedsTheRoot(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	sc, resumed, err := s.BeginScan(ctx, p.ID, false)
	if err != nil {
		t.Fatalf("BeginScan: %v", err)
	}
	if resumed {
		t.Error("a first scan reported itself as resumed")
	}
	if sc.ID == 0 || sc.PairID != p.ID || sc.State != ScanRunning || sc.StartedAt.IsZero() {
		t.Fatalf("BeginScan returned %+v", sc)
	}

	if dirs := pendingDirs(t, s, p.ID, sc.ID); !slices.Equal(dirs, []string{""}) {
		t.Fatalf("frontier = %q, want the root", dirs)
	}
	root, ok, err := s.ManifestEntryAt(ctx, p.ID, "")
	if err != nil {
		t.Fatalf("ManifestEntryAt: %v", err)
	}
	if !ok || !root.IsDir {
		t.Errorf("root row = %+v (found %v), want an unlisted directory", root, ok)
	}
}

func TestPutListingRecordsChildrenAndCounters(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	sc, _, err := s.BeginScan(ctx, p.ID, false)
	if err != nil {
		t.Fatalf("BeginScan: %v", err)
	}

	if err := s.PutListing(ctx, p.ID, sc.ID, "", []ManifestEntry{
		{Path: "sub", IsDir: true},
		{Path: "a.mkv", Size: 10, MTime: manifestTime},
		{Path: "b.mkv", Size: 5, MTime: manifestTime},
	}); err != nil {
		t.Fatalf("PutListing: %v", err)
	}

	if dirs := pendingDirs(t, s, p.ID, sc.ID); !slices.Equal(dirs, []string{"sub"}) {
		t.Fatalf("frontier = %q, want [sub]: the root is listed, its subdirectory is not", dirs)
	}

	if err := s.PutListing(ctx, p.ID, sc.ID, "sub", []ManifestEntry{
		{Path: "sub/c.mkv", Size: 7, MTime: manifestTime},
	}); err != nil {
		t.Fatalf("PutListing: %v", err)
	}
	if dirs := pendingDirs(t, s, p.ID, sc.ID); len(dirs) != 0 {
		t.Fatalf("frontier = %q, want it exhausted", dirs)
	}

	got, err := s.ScanByID(ctx, sc.ID)
	if err != nil {
		t.Fatalf("ScanByID: %v", err)
	}
	if got.Dirs != 2 || got.Files != 3 || got.Bytes != 22 {
		t.Errorf("counters = %d dirs, %d files, %d bytes; want 2, 3, 22", got.Dirs, got.Files, got.Bytes)
	}

	files, bytes, err := s.ManifestStats(ctx, p.ID)
	if err != nil {
		t.Fatalf("ManifestStats: %v", err)
	}
	if files != 3 || bytes != 22 {
		t.Errorf("manifest stats = %d files, %d bytes; want 3, 22", files, bytes)
	}

	e, ok, err := s.ManifestEntryAt(ctx, p.ID, "a.mkv")
	if err != nil || !ok {
		t.Fatalf("ManifestEntryAt: %v (found %v)", err, ok)
	}
	if e.Size != 10 || e.IsDir || !e.MTime.Equal(manifestTime) {
		t.Errorf("a.mkv = %+v, want size 10 at %v", e, manifestTime)
	}

	if _, ok, err := s.ManifestEntryAt(ctx, p.ID, "nothing.mkv"); err != nil || ok {
		t.Errorf("ManifestEntryAt of an unknown path = %v (err %v), want not found", ok, err)
	}
}

func TestPendingDirsIsLimited(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	sc, _, err := s.BeginScan(ctx, p.ID, false)
	if err != nil {
		t.Fatalf("BeginScan: %v", err)
	}
	if err := s.PutListing(ctx, p.ID, sc.ID, "", []ManifestEntry{
		{Path: "c", IsDir: true}, {Path: "a", IsDir: true}, {Path: "b", IsDir: true},
	}); err != nil {
		t.Fatalf("PutListing: %v", err)
	}

	dirs, err := s.PendingDirs(ctx, p.ID, sc.ID, 2)
	if err != nil {
		t.Fatalf("PendingDirs: %v", err)
	}
	if !slices.Equal(dirs, []string{"a", "b"}) {
		t.Errorf("frontier = %q, want the first two in path order", dirs)
	}
}

func TestBeginScanResumesAnUnfinishedWalk(t *testing.T) {
	for _, prior := range []string{ScanRunning, ScanInterrupted} {
		t.Run("after "+prior, func(t *testing.T) {
			ctx := context.Background()
			s := newStore(t)
			p := newPair(t, s, "media")

			first, _, err := s.BeginScan(ctx, p.ID, false)
			if err != nil {
				t.Fatalf("BeginScan: %v", err)
			}
			if err := s.PutListing(ctx, p.ID, first.ID, "", []ManifestEntry{
				{Path: "sub", IsDir: true},
				{Path: "a.mkv", Size: 10, MTime: manifestTime},
			}); err != nil {
				t.Fatalf("PutListing: %v", err)
			}
			if prior == ScanInterrupted {
				if _, err := s.FinishScan(ctx, first.ID, ScanInterrupted, "transfer window closed"); err != nil {
					t.Fatalf("FinishScan: %v", err)
				}
			}

			second, resumed, err := s.BeginScan(ctx, p.ID, true)
			if err != nil {
				t.Fatalf("BeginScan(resume): %v", err)
			}
			if !resumed || second.ID != first.ID {
				t.Fatalf("resume returned scan %d (resumed=%v), want %d", second.ID, resumed, first.ID)
			}
			if second.Dirs != 1 {
				t.Errorf("resumed scan has %d dirs, want the 1 the first walk listed", second.Dirs)
			}
			// The frontier survives the restart: the root stays listed and only the
			// directory the walk never got to is still pending.
			if dirs := pendingDirs(t, s, p.ID, second.ID); !slices.Equal(dirs, []string{"sub"}) {
				t.Errorf("frontier after resume = %q, want [sub]", dirs)
			}
		})
	}
}

// An interrupted walk has not finished, so it keeps finished_at empty: the next
// run continues this row rather than starting another.
func TestFinishScanInterruptedKeepsTheRowOpen(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	sc, _, err := s.BeginScan(ctx, p.ID, false)
	if err != nil {
		t.Fatalf("BeginScan: %v", err)
	}
	if _, err := s.FinishScan(ctx, sc.ID, ScanInterrupted, "transfer window closed"); err != nil {
		t.Fatalf("FinishScan: %v", err)
	}

	got, err := s.ScanByID(ctx, sc.ID)
	if err != nil {
		t.Fatalf("ScanByID: %v", err)
	}
	if got.State != ScanInterrupted || got.FinishedAt != nil {
		t.Errorf("interrupted scan = %+v, want no finished_at", got)
	}
	if got.Error != "transfer window closed" {
		t.Errorf("error = %q", got.Error)
	}
}

func TestBeginScanWithoutResumeSupersedes(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	first, _, err := s.BeginScan(ctx, p.ID, false)
	if err != nil {
		t.Fatalf("BeginScan: %v", err)
	}
	if err := s.PutListing(ctx, p.ID, first.ID, "", []ManifestEntry{{Path: "sub", IsDir: true}}); err != nil {
		t.Fatalf("PutListing: %v", err)
	}

	second, resumed, err := s.BeginScan(ctx, p.ID, false)
	if err != nil {
		t.Fatalf("BeginScan: %v", err)
	}
	if resumed || second.ID == first.ID {
		t.Fatalf("second scan = %d (resumed=%v), want a new one", second.ID, resumed)
	}

	// Two walks writing the same manifest would sweep each other's rows, so the
	// older one is closed rather than left running.
	old, err := s.ScanByID(ctx, first.ID)
	if err != nil {
		t.Fatalf("ScanByID: %v", err)
	}
	if old.State != ScanFailed {
		t.Errorf("superseded scan state = %q, want %q", old.State, ScanFailed)
	}
	if old.Error == "" || old.FinishedAt == nil {
		t.Errorf("superseded scan = %+v, want an error and a finish time", old)
	}

	running, ok, err := s.RunningScan(ctx, p.ID)
	if err != nil || !ok {
		t.Fatalf("RunningScan: %v (found %v)", err, ok)
	}
	if running.ID != second.ID {
		t.Errorf("RunningScan = %d, want %d", running.ID, second.ID)
	}

	// The new walk starts at the root again; "sub" belongs to the abandoned scan
	// and must not be handed out as this one's frontier.
	if dirs := pendingDirs(t, s, p.ID, second.ID); !slices.Equal(dirs, []string{""}) {
		t.Errorf("frontier = %q, want the root only", dirs)
	}
}

// The asymmetry below is the deletion-safety property of DESIGN.md §2.5: a walk
// that stopped early has not seen most of the tree, so what it is missing is not
// what the publisher deleted. Only a completed walk may sweep - sweeping on a
// failure turns one unreadable directory into "everything was deleted", and the
// deletion of a 30 TB dataset is the one operation here that is irrecoverable.
func TestFinishScanSweepsOnlyWhenTheWalkCompleted(t *testing.T) {
	seen := []ManifestEntry{
		{Path: "a.mkv", Size: 1, MTime: manifestTime},
		{Path: "b.mkv", Size: 2, MTime: manifestTime},
	}
	all := append(slices.Clone(seen), ManifestEntry{Path: "c.mkv", Size: 3, MTime: manifestTime})

	partialWalk := func(t *testing.T, s *Store, pairID int64) int64 {
		t.Helper()
		ctx := context.Background()
		sc, _, err := s.BeginScan(ctx, pairID, false)
		if err != nil {
			t.Fatalf("BeginScan: %v", err)
		}
		if err := s.PutListing(ctx, pairID, sc.ID, "", seen); err != nil {
			t.Fatalf("PutListing: %v", err)
		}
		return sc.ID
	}

	for _, state := range []string{ScanFailed, ScanInterrupted} {
		t.Run("no sweep after "+state, func(t *testing.T) {
			ctx := context.Background()
			s := newStore(t)
			p := newPair(t, s, "media")
			putManifest(t, s, p.ID, all...)

			id := partialWalk(t, s, p.ID)
			swept, err := s.FinishScan(ctx, id, state, "connection reset by peer")
			if err != nil {
				t.Fatalf("FinishScan: %v", err)
			}
			if swept != 0 {
				t.Errorf("a %s scan swept %d rows, want 0", state, swept)
			}
			if _, ok, err := s.ManifestEntryAt(ctx, p.ID, "c.mkv"); err != nil || !ok {
				t.Errorf("a %s scan dropped c.mkv from the manifest (err %v)", state, err)
			}
			if files, _, err := s.ManifestStats(ctx, p.ID); err != nil || files != 3 {
				t.Errorf("manifest holds %d files after a %s scan, want 3 (err %v)", files, state, err)
			}
		})
	}

	t.Run("sweep after done", func(t *testing.T) {
		ctx := context.Background()
		s := newStore(t)
		p := newPair(t, s, "media")
		putManifest(t, s, p.ID, all...)

		id := partialWalk(t, s, p.ID)
		swept, err := s.FinishScan(ctx, id, ScanDone, "")
		if err != nil {
			t.Fatalf("FinishScan: %v", err)
		}
		if swept != 1 {
			t.Errorf("a completed scan swept %d rows, want 1", swept)
		}
		if _, ok, err := s.ManifestEntryAt(ctx, p.ID, "c.mkv"); err != nil || ok {
			t.Errorf("a completed scan kept a file the publisher no longer has (err %v)", err)
		}
		if files, _, err := s.ManifestStats(ctx, p.ID); err != nil || files != 2 {
			t.Errorf("manifest holds %d files after a completed scan, want 2 (err %v)", files, err)
		}

		got, err := s.ScanByID(ctx, id)
		if err != nil {
			t.Fatalf("ScanByID: %v", err)
		}
		if got.State != ScanDone || got.FinishedAt == nil || got.Error != "" {
			t.Errorf("finished scan = %+v, want a clean done row", got)
		}
	})
}

func TestLastCompletedScan(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	if _, ok, err := s.LastCompletedScan(ctx, p.ID); err != nil || ok {
		t.Fatalf("LastCompletedScan before any walk = %v (err %v), want none", ok, err)
	}

	putManifest(t, s, p.ID, ManifestEntry{Path: "a.mkv", Size: 1, MTime: manifestTime})
	done, ok, err := s.LastCompletedScan(ctx, p.ID)
	if err != nil || !ok {
		t.Fatalf("LastCompletedScan: %v (found %v)", err, ok)
	}

	// A later walk that did not complete does not become the differ's answer.
	failed, _, err := s.BeginScan(ctx, p.ID, false)
	if err != nil {
		t.Fatalf("BeginScan: %v", err)
	}
	if _, err := s.FinishScan(ctx, failed.ID, ScanFailed, "no route to host"); err != nil {
		t.Fatalf("FinishScan: %v", err)
	}
	got, ok, err := s.LastCompletedScan(ctx, p.ID)
	if err != nil || !ok {
		t.Fatalf("LastCompletedScan: %v (found %v)", err, ok)
	}
	if got.ID != done.ID {
		t.Errorf("LastCompletedScan = %d, want %d", got.ID, done.ID)
	}
}

func TestScansAreNewestFirst(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	var ids []int64
	for i := 0; i < 3; i++ {
		sc, _, err := s.BeginScan(ctx, p.ID, false)
		if err != nil {
			t.Fatalf("BeginScan: %v", err)
		}
		if _, err := s.FinishScan(ctx, sc.ID, ScanDone, ""); err != nil {
			t.Fatalf("FinishScan: %v", err)
		}
		ids = append(ids, sc.ID)
	}

	scans, err := s.Scans(ctx, p.ID, 0)
	if err != nil {
		t.Fatalf("Scans: %v", err)
	}
	if len(scans) != 3 || scans[0].ID != ids[2] {
		t.Fatalf("Scans returned %d rows, newest %d, want 3 and %d", len(scans), scans[0].ID, ids[2])
	}

	limited, err := s.Scans(ctx, p.ID, 1)
	if err != nil {
		t.Fatalf("Scans: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("limit 1 returned %d rows", len(limited))
	}
}

func TestUnknownScanIsAnError(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.ScanByID(ctx, 99); err == nil {
		t.Error("ScanByID of an unknown scan succeeded, want an error")
	}
	if _, err := s.FinishScan(ctx, 99, ScanDone, ""); err == nil {
		t.Error("FinishScan of an unknown scan succeeded, want an error")
	}
}
