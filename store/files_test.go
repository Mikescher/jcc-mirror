package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func putLocal(t *testing.T, s *Store, pairID int64, path string, size int64, mtime time.Time) {
	t.Helper()
	if err := s.PutFile(context.Background(), File{PairID: pairID, Path: path, Size: size, MTime: mtime}); err != nil {
		t.Fatalf("PutFile(%q): %v", path, err)
	}
}

func TestPutFileUpserts(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	putLocal(t, s, p.ID, "a.mkv", 100, manifestTime)

	f, ok, err := s.FileAt(ctx, p.ID, "a.mkv")
	if err != nil || !ok {
		t.Fatalf("FileAt: %v (found %v)", err, ok)
	}
	if f.Size != 100 || !f.MTime.Equal(manifestTime) {
		t.Errorf("file = %+v, want size 100 at %v", f, manifestTime)
	}
	if f.State != FileSynced {
		t.Errorf("state = %q, want %q", f.State, FileSynced)
	}
	if f.VerifiedAt.IsZero() {
		t.Error("verified_at was left empty")
	}

	later := manifestTime.Add(time.Hour)
	if err := s.PutFile(ctx, File{
		PairID: p.ID, Path: "a.mkv", Size: 200, MTime: later,
		Hash: "deadbeef", State: FileAdopted, VerifiedAt: later,
	}); err != nil {
		t.Fatalf("PutFile: %v", err)
	}

	f, ok, err = s.FileAt(ctx, p.ID, "a.mkv")
	if err != nil || !ok {
		t.Fatalf("FileAt: %v (found %v)", err, ok)
	}
	if f.Size != 200 || f.Hash != "deadbeef" || f.State != FileAdopted || !f.MTime.Equal(later) {
		t.Errorf("file after the second write = %+v", f)
	}

	count, bytes, err := s.FileStats(ctx, p.ID)
	if err != nil {
		t.Fatalf("FileStats: %v", err)
	}
	if count != 1 || bytes != 200 {
		t.Errorf("stats = %d files, %d bytes; want 1, 200 (the row was replaced, not duplicated)", count, bytes)
	}
}

func TestFileAtMissing(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	f, ok, err := s.FileAt(ctx, p.ID, "nothing.mkv")
	if err != nil {
		t.Fatalf("FileAt: %v", err)
	}
	if ok || f.Path != "" {
		t.Errorf("FileAt of an unknown path = %+v (found %v)", f, ok)
	}
}

func TestPutFilesBatch(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	if err := s.PutFiles(ctx, nil); err != nil {
		t.Fatalf("PutFiles(nil): %v", err)
	}

	if err := s.PutFiles(ctx, []File{
		{PairID: p.ID, Path: "a.mkv", Size: 10, MTime: manifestTime, State: FileAdopted},
		{PairID: p.ID, Path: "b.mkv", Size: 20, MTime: manifestTime, State: FileAdopted},
		{PairID: p.ID, Path: "c.mkv", Size: 30, MTime: manifestTime, State: FileAdopted},
	}); err != nil {
		t.Fatalf("PutFiles: %v", err)
	}

	count, bytes, err := s.FileStats(ctx, p.ID)
	if err != nil {
		t.Fatalf("FileStats: %v", err)
	}
	if count != 3 || bytes != 60 {
		t.Errorf("stats = %d files, %d bytes; want 3, 60", count, bytes)
	}
}

// The batch is one transaction: half an adopted bootstrap would claim files as
// local truth that were never matched against the remote.
func TestPutFilesIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	err := s.PutFiles(ctx, []File{
		{PairID: p.ID, Path: "a.mkv", Size: 10, MTime: manifestTime},
		{PairID: p.ID, Path: "b.mkv", Size: 20, MTime: manifestTime, State: "guessed"},
	})
	if err == nil {
		t.Fatal("PutFiles accepted an unknown file state, want an error")
	}
	if _, ok, err := s.FileAt(ctx, p.ID, "a.mkv"); err != nil || ok {
		t.Errorf("the valid half of a rejected batch was written (err %v)", err)
	}
}

func TestForgetFile(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	putLocal(t, s, p.ID, "a.mkv", 100, manifestTime)
	if err := s.ForgetFile(ctx, p.ID, "a.mkv"); err != nil {
		t.Fatalf("ForgetFile: %v", err)
	}
	if _, ok, err := s.FileAt(ctx, p.ID, "a.mkv"); err != nil || ok {
		t.Errorf("the row survived ForgetFile (found %v, err %v)", ok, err)
	}
	if err := s.ForgetFile(ctx, p.ID, "a.mkv"); err != nil {
		t.Errorf("forgetting an unknown file = %v, want no error", err)
	}
}

func TestChangesFilter(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")
	other := newPair(t, s, "jcc")

	before, after := int64(10), int64(20)
	old := time.Now().Add(-48 * time.Hour)
	for _, c := range []Change{
		{PairID: p.ID, Path: "a.mkv", Op: ChangeAdd, SizeAfter: &after, TS: old},
		{PairID: p.ID, Path: "b.mkv", Op: ChangeReplace, SizeBefore: &before, SizeAfter: &after},
		{PairID: p.ID, Path: "c.mkv", Op: ChangeFail, Error: "connection reset by peer"},
		{PairID: other.ID, Path: "d.mkv", Op: ChangeAdd, SizeAfter: &after},
	} {
		c := c
		if err := s.AppendChange(ctx, &c); err != nil {
			t.Fatalf("AppendChange %q: %v", c.Path, err)
		}
		if c.ID == 0 || c.TS.IsZero() {
			t.Fatalf("AppendChange %q left id=%d ts=%v", c.Path, c.ID, c.TS)
		}
	}

	all, err := s.Changes(ctx, ChangeFilter{})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("got %d changes, want 4", len(all))
	}
	if all[0].Path != "d.mkv" {
		t.Errorf("newest change = %q, want d.mkv", all[0].Path)
	}
	if all[0].SizeBefore != nil || all[0].SizeAfter == nil || *all[0].SizeAfter != after {
		t.Errorf("an add recorded sizes %v -> %v, want none -> %d", all[0].SizeBefore, all[0].SizeAfter, after)
	}

	mine, err := s.Changes(ctx, ChangeFilter{PairID: &p.ID})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(mine) != 3 {
		t.Errorf("pair filter returned %d changes, want 3", len(mine))
	}

	failures, err := s.Changes(ctx, ChangeFilter{Ops: []string{ChangeFail}})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(failures) != 1 || failures[0].Error != "connection reset by peer" {
		t.Errorf("op filter returned %d changes", len(failures))
	}

	recent, err := s.Changes(ctx, ChangeFilter{Since: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(recent) != 3 {
		t.Errorf("since filter returned %d changes, want 3", len(recent))
	}

	limited, err := s.Changes(ctx, ChangeFilter{Limit: 2})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("limit 2 returned %d changes", len(limited))
	}

	n, err := s.PruneChanges(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("PruneChanges: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d changes, want 1", n)
	}
}

func collectChanged(t *testing.T, s *Store, pairID int64, tolerance time.Duration) []DiffRow {
	t.Helper()
	var out []DiffRow
	if err := s.ChangedFiles(context.Background(), pairID, tolerance, func(d DiffRow) error {
		out = append(out, d)
		return nil
	}); err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	return out
}

// The manifest stores mtimes as milliseconds, while the local timestamp carries
// whatever granularity its filesystem has. Comparing for equality would make
// every file look changed on every scan, forever, and re-transfer 30 TB
// (DESIGN.md §2.3).
func TestChangedFilesMtimeTolerance(t *testing.T) {
	const tolerance = 2 * time.Second

	cases := []struct {
		name      string
		skew      time.Duration
		localSize int64
		want      bool
	}{
		{"the same instant", 0, 100, false},
		{"a second of clock skew", time.Second, 100, false},
		{"a second early", -time.Second, 100, false},
		{"exactly the tolerance", tolerance, 100, false},
		{"past the tolerance", tolerance + time.Second, 100, true},
		{"past the tolerance, early", -(tolerance + time.Second), 100, true},
		{"a different size at the same instant", 0, 99, true},
		{"a different size within the tolerance", time.Second, 99, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStore(t)
			p := newPair(t, s, "media")
			putManifest(t, s, p.ID, ManifestEntry{Path: "a.mkv", Size: 100, MTime: manifestTime})
			putLocal(t, s, p.ID, "a.mkv", c.localSize, manifestTime.Add(c.skew))

			changed := collectChanged(t, s, p.ID, tolerance)
			if got := len(changed) > 0; got != c.want {
				t.Errorf("changed = %v, want %v (rows %+v)", got, c.want, changed)
			}
		})
	}
}

func TestChangedFilesReportsAddsAndReplaces(t *testing.T) {
	s := newStore(t)
	p := newPair(t, s, "media")

	putManifest(t, s, p.ID,
		ManifestEntry{Path: "new.mkv", Size: 10, MTime: manifestTime},
		ManifestEntry{Path: "old.mkv", Size: 20, MTime: manifestTime})
	putLocal(t, s, p.ID, "old.mkv", 5, manifestTime)

	changed := collectChanged(t, s, p.ID, 2*time.Second)
	if len(changed) != 2 {
		t.Fatalf("got %d changed files, want 2: %+v", len(changed), changed)
	}

	add := changed[0]
	if add.Path != "new.mkv" || add.Have || add.LocalSize != 0 {
		t.Errorf("add = %+v, want new.mkv with nothing local", add)
	}
	if add.Size != 10 || !add.MTime.Equal(manifestTime) {
		t.Errorf("add carries %d bytes at %v, want the publisher's 10 at %v", add.Size, add.MTime, manifestTime)
	}

	replace := changed[1]
	if replace.Path != "old.mkv" || !replace.Have || replace.LocalSize != 5 || replace.Size != 20 {
		t.Errorf("replace = %+v, want old.mkv held at 5 bytes against 20", replace)
	}
}

func TestChangedFilesSkipsDirectories(t *testing.T) {
	s := newStore(t)
	p := newPair(t, s, "media")

	putManifest(t, s, p.ID,
		ManifestEntry{Path: "sub", IsDir: true},
		ManifestEntry{Path: "sub/a.mkv", Size: 10, MTime: manifestTime})

	changed := collectChanged(t, s, p.ID, 2*time.Second)
	if len(changed) != 1 || changed[0].Path != "sub/a.mkv" {
		t.Errorf("changed = %+v, want the file only", changed)
	}
}

func TestChangedFilesPropagatesCallbackErrors(t *testing.T) {
	s := newStore(t)
	p := newPair(t, s, "media")
	putManifest(t, s, p.ID,
		ManifestEntry{Path: "a.mkv", Size: 10, MTime: manifestTime},
		ManifestEntry{Path: "b.mkv", Size: 20, MTime: manifestTime})

	stop := errors.New("out of disk")
	seen := 0
	err := s.ChangedFiles(context.Background(), p.ID, 2*time.Second, func(DiffRow) error {
		seen++
		return stop
	})
	if !errors.Is(err, stop) {
		t.Errorf("ChangedFiles error = %v, want the callback's", err)
	}
	if seen != 1 {
		t.Errorf("the callback ran %d times after refusing the first row", seen)
	}
}

func TestVanishedFiles(t *testing.T) {
	s := newStore(t)
	p := newPair(t, s, "media")

	putManifest(t, s, p.ID,
		ManifestEntry{Path: "kept.mkv", Size: 10, MTime: manifestTime},
		// A path the publisher now has as a directory is not a file we still hold a
		// match for.
		ManifestEntry{Path: "turned-into-a-dir", IsDir: true})
	putLocal(t, s, p.ID, "kept.mkv", 10, manifestTime)
	putLocal(t, s, p.ID, "gone.mkv", 42, manifestTime)
	putLocal(t, s, p.ID, "turned-into-a-dir", 7, manifestTime)

	var paths []string
	var total int64
	if err := s.VanishedFiles(context.Background(), p.ID, func(path string, size int64) error {
		paths = append(paths, path)
		total += size
		return nil
	}); err != nil {
		t.Fatalf("VanishedFiles: %v", err)
	}

	if want := []string{"gone.mkv", "turned-into-a-dir"}; !slices.Equal(paths, want) {
		t.Errorf("vanished = %q, want %q", paths, want)
	}
	if total != 49 {
		t.Errorf("vanished bytes = %d, want 49", total)
	}
}

func TestRemoteFilesStreamsFilesOnly(t *testing.T) {
	s := newStore(t)
	p := newPair(t, s, "media")

	putManifest(t, s, p.ID,
		ManifestEntry{Path: "sub", IsDir: true},
		ManifestEntry{Path: "b.mkv", Size: 20, MTime: manifestTime},
		ManifestEntry{Path: "a.mkv", Size: 10, MTime: manifestTime})

	var got []ManifestEntry
	if err := s.RemoteFiles(context.Background(), p.ID, func(e ManifestEntry) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatalf("RemoteFiles: %v", err)
	}

	// The seeded root is a directory too, so a stream that leaked directories would
	// hand adoption an entry with an empty path.
	if len(got) != 2 {
		t.Fatalf("RemoteFiles streamed %d entries, want the 2 files: %+v", len(got), got)
	}
	if got[0].Path != "a.mkv" || got[1].Path != "b.mkv" {
		t.Errorf("paths = %q, %q; want them in path order", got[0].Path, got[1].Path)
	}
	if got[0].Size != 10 || !got[0].MTime.Equal(manifestTime) {
		t.Errorf("a.mkv = %+v, want size 10 at %v", got[0], manifestTime)
	}
}
