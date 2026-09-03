package engine

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"blackforestbytes.com/jcc-mirror/remote"
	"blackforestbytes.com/jcc-mirror/remote/localfs"
	"blackforestbytes.com/jcc-mirror/store"
)

// The database of the harness's jcc pair, and the lock that gates it.
const (
	testDB   = "ClipCornDB.db"
	testLock = "ClipCornDB.db" + LockSuffix
)

// newJCC is the harness with its pair typed jcc. Everything in DESIGN.md §3 -
// the hard exclusions and the lock gate - applies to that type and to nothing
// else, so a test of it has to start here.
func newJCC(t *testing.T, opts ...func(*Options)) *harness {
	t.Helper()

	h := newHarness(t, opts...)
	h.pair.Type = store.PairJCC
	if err := h.store.UpdatePair(context.Background(), &h.pair); err != nil {
		t.Fatalf("make the pair a jcc one: %v", err)
	}
	return h
}

// lockRemote puts the publisher's lock file in place, as jClipCorn does when it
// opens the database over there.
func (h *harness) lockRemote() {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.src, testLock), []byte("4711"), 0o644); err != nil {
		h.t.Fatalf("write the remote lock: %v", err)
	}
}

func (h *harness) unlockRemote() {
	h.t.Helper()
	if err := os.Remove(filepath.Join(h.src, testLock)); err != nil {
		h.t.Fatalf("remove the remote lock: %v", err)
	}
}

// syncDB runs the gate.
func (h *harness) syncDB(force bool) DBResult {
	h.t.Helper()

	res, err := h.engine.SyncDatabase(context.Background(), h.pair, force)
	if err != nil {
		h.t.Fatalf("SyncDatabase: %v", err)
	}
	return res
}

// dbHere is what the destination holds for the database right now.
func (h *harness) dbHere() []byte {
	h.t.Helper()

	body, err := os.ReadFile(filepath.Join(h.dst, testDB))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		h.t.Fatalf("read the local database: %v", err)
	}
	return body
}

// wantNoIncoming checks that no half-copied database was left beside the real
// one. The staging file lives in the target directory, so one that survives is
// visible to anything looking at that directory.
func (h *harness) wantNoIncoming() {
	h.t.Helper()

	entries, err := os.ReadDir(h.dst)
	if err != nil {
		h.t.Fatalf("read the destination: %v", err)
	}
	for _, e := range entries {
		if len(e.Name()) > 0 && e.Name()[0] == '.' && filepath.Ext(e.Name()) == incomingSuffix {
			h.t.Errorf("%s was left behind", e.Name())
		}
	}
}

func (h *harness) backups() []store.DBBackup {
	h.t.Helper()

	all, err := h.store.DBBackups(context.Background(), h.pair.ID, 0)
	if err != nil {
		h.t.Fatalf("DBBackups: %v", err)
	}
	return all
}

// TestJCCRefusals is the hard-exclusion table of DESIGN.md §3, which is a set of
// refusals rather than patterns because no configuration makes any of them
// right.
func TestJCCRefusals(t *testing.T) {
	j := JCC{}.fill()

	refused := []string{
		"ClipCornUserData.db",
		"ClipCornHistory.db",
		"sub/ClipCornUserData.db",
		"ClipCornDB.db-journal",
		"ClipCornDB.db-wal",
		"ClipCornDB.db-shm",
		"ClipCornDB.db.~lock",
		"ClipCornUserData.db.~lock",
		".ClipCornDB.db.incoming",
	}
	for _, rel := range refused {
		if j.refuses(rel) == "" {
			t.Errorf("%q is transferred, and it must never be", rel)
		}
	}

	allowed := []string{"ClipCornDB.db", "cover/a.jpg", "cover/ünïcode.png", "readme.txt"}
	for _, rel := range allowed {
		if why := j.refuses(rel); why != "" {
			t.Errorf("%q is refused (%s), and it should transfer", rel, why)
		}
	}
}

// The per-user databases are the whole reason the refusals exist: copying the
// publisher's over would destroy this side's ratings, tags and history.
func TestJCCNeverTransfersThePerUserDatabases(t *testing.T) {
	h := newJCC(t)

	h.write(testDB, 2048)
	h.write("ClipCornUserData.db", 512)
	h.write("ClipCornHistory.db", 512)
	h.write("ClipCornDB.db-journal", 128)
	h.write("cover/a.jpg", 64)
	h.lockRemote()

	mine := []byte("this side's own ratings")
	h.local("ClipCornUserData.db", mine, time.Time{})

	h.scan()

	manifest := h.manifest(h.pair)
	for _, rel := range []string{"ClipCornUserData.db", "ClipCornHistory.db", "ClipCornDB.db-journal", testLock} {
		if _, ok := manifest[rel]; ok {
			t.Errorf("%q reached the manifest; the walk must drop it", rel)
		}
	}
	if _, ok := manifest[testDB]; !ok {
		t.Errorf("%q is missing from the manifest: it is gated, not excluded", testDB)
	}

	h.sync()

	if got := string(h.dbHere()); got != "" {
		t.Errorf("the database was transferred by the queue: %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(h.dst, "ClipCornUserData.db")); err != nil {
		t.Fatalf("read our own user data: %v", err)
	} else if string(got) != string(mine) {
		t.Errorf("ClipCornUserData.db was overwritten with the publisher's copy")
	}
	h.wantFile("cover/a.jpg", srcBytes(h, "cover/a.jpg"))
}

// srcBytes re-reads a file from the publisher's side, for an assertion that did
// not write it itself.
func srcBytes(h *harness, rel string) []byte {
	h.t.Helper()

	body, err := os.ReadFile(filepath.Join(h.src, filepath.FromSlash(rel)))
	if err != nil {
		h.t.Fatalf("read %s: %v", rel, err)
	}
	return body
}

// The database is counted by the plan and never queued: it is copied between two
// pairs of lock probes, which is not something a job's retry budget and .part
// file can express.
func TestJCCDatabaseIsPlannedButNotQueued(t *testing.T) {
	h := newJCC(t)
	h.write(testDB, 4096)
	h.write("cover/a.jpg", 100)
	h.scan()

	plan, err := h.engine.Enqueue(context.Background(), h.pair)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if plan.Add != 2 {
		t.Errorf("plan adds %d files, want 2 (the database is counted like any other)", plan.Add)
	}
	if plan.Database == nil {
		t.Fatal("the plan does not name the database")
	}
	if plan.Database.Path != testDB {
		t.Errorf("plan names %q as the database, want %q", plan.Database.Path, testDB)
	}
	if plan.Queued != 1 {
		t.Errorf("%d job(s) queued, want 1: the database is not one of them", plan.Queued)
	}
	for _, j := range h.jobs() {
		if j.Path == testDB {
			t.Errorf("the database was queued as job %d", j.ID)
		}
	}
}

// The gate as §3 writes it, with neither side holding a lock.
func TestJCCGateCopiesTheDatabase(t *testing.T) {
	h := newJCC(t)
	want := h.write(testDB, 4096)
	h.scan()

	res := h.syncDB(false)
	if !res.Replaced {
		t.Fatalf("the database was not replaced: %+v", res)
	}
	if res.Backup != nil {
		t.Errorf("a backup was kept although there was nothing here to replace")
	}
	h.wantFile(testDB, want)
	h.wantNoIncoming()

	// Local truth has to know, or the next walk plans the same copy again.
	file, ok, err := h.store.FileAt(context.Background(), h.pair.ID, testDB)
	if err != nil || !ok {
		t.Fatalf("no row of local truth for the database: %v", err)
	}
	if file.Size != int64(len(want)) {
		t.Errorf("local truth says %d bytes, want %d", file.Size, len(want))
	}

	h.scan()
	plan, err := h.engine.Plan(context.Background(), h.pair, 10)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Transfers() != 0 {
		t.Errorf("the second plan still wants %d transfer(s): %+v", plan.Transfers(), plan.Entries)
	}

	// And a second run of the gate has nothing to do rather than copying again.
	again := h.syncDB(false)
	if again.Replaced || again.Skipped == "" {
		t.Errorf("the second run did not recognise the database as already here: %+v", again)
	}
}

// Either lock stops the copy, and the answer is "not this cycle" rather than a
// failure.
func TestJCCGateDefersWhileALockIsHeld(t *testing.T) {
	for _, side := range []string{"publisher", "here"} {
		t.Run(side, func(t *testing.T) {
			h := newJCC(t)
			old := []byte("the database that is already here")
			h.local(testDB, old, time.Time{})
			h.write(testDB, 4096)
			h.scan()

			if side == "publisher" {
				h.lockRemote()
			} else {
				h.local(testLock, []byte("4711"), time.Time{})
			}

			res := h.syncDB(false)
			if res.Replaced {
				t.Fatal("the database was copied although a lock was held")
			}
			if res.Deferred == "" {
				t.Errorf("nothing said why it was left alone: %+v", res)
			}
			if got := string(h.dbHere()); got != string(old) {
				t.Errorf("the database here changed: %q", got)
			}
			h.wantNoIncoming()

			// "Skip this cycle" means exactly that: the next one copies it, with
			// nothing else having to happen in between.
			if side == "publisher" {
				h.unlockRemote()
			} else if err := os.Remove(filepath.Join(h.dst, testLock)); err != nil {
				t.Fatalf("remove the local lock: %v", err)
			}
			if next := h.syncDB(false); !next.Replaced {
				t.Errorf("the cycle after the lock cleared did not copy the database: %+v", next)
			}
		})
	}
}

// S1, the one non-obvious step: user 1 can launch jClipCorn while the copy runs,
// and one extra request each side is what closes that window.
func TestJCCGateDiscardsWhenTheLockAppearsMidCopy(t *testing.T) {
	h := newJCC(t)
	old := []byte("the database that is already here")
	h.local(testDB, old, time.Time{})
	h.write(testDB, 4096)
	h.scan()

	fake := h.wrap()
	fake.onRange = func(p string, _, _ int64) error {
		if p == testDB {
			h.lockRemote() // jClipCorn started over there, mid-copy
		}
		return nil
	}

	res := h.syncDB(false)
	if res.Replaced {
		t.Fatal("a database copied while the publisher had it open was kept")
	}
	if res.Deferred == "" {
		t.Errorf("nothing said why it was discarded: %+v", res)
	}
	if got := string(h.dbHere()); got != string(old) {
		t.Errorf("the database here changed: %q", got)
	}
	h.wantNoIncoming()
	if len(h.backups()) != 0 {
		t.Errorf("a backup was taken for a copy that was thrown away")
	}
}

// The size check of step 4. A short copy is the one failure everything
// downstream would accept in silence.
func TestJCCGateRefusesAShortCopy(t *testing.T) {
	h := newJCC(t)
	old := []byte("the database that is already here")
	h.local(testDB, old, time.Time{})
	h.write(testDB, 4096)
	h.scan()

	h.engine.remote = shortRemote{FS: h.engine.remote.(*localfs.FS), path: testDB}

	_, err := h.engine.SyncDatabase(context.Background(), h.pair, false)
	if err == nil {
		t.Fatal("a short copy was accepted")
	}
	if got := string(h.dbHere()); got != string(old) {
		t.Errorf("the database here changed: %q", got)
	}
	h.wantNoIncoming()
}

// shortRemote hands back fewer bytes than it said the file had, which is what a
// connection dropped mid-response looks like.
type shortRemote struct {
	*localfs.FS
	path string
}

func (s shortRemote) OpenRange(ctx context.Context, p string, off, length int64) (io.ReadCloser, int64, error) {
	rc, total, err := s.FS.OpenRange(ctx, p, off, length)
	if err != nil || p != s.path {
		return rc, total, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(rc, total/2), rc}, total, nil
}

var _ remote.RangeReader = shortRemote{}

// S5: the copy that is replaced is kept, and the keep count is what stops that
// from growing without bound.
func TestJCCGateKeepsNumberedBackups(t *testing.T) {
	h := newJCC(t, func(o *Options) { o.JCC.Backups = 2 })

	// Four rounds against a keep count of two: the first leaves nothing to keep,
	// the next three each keep what they replaced, and one of those three has to
	// go again.
	var bodies [][]byte
	for i := 0; i < 4; i++ {
		bodies = append(bodies, h.write(testDB, 1024+i))
		h.scan()
		res := h.syncDB(false)
		if !res.Replaced {
			t.Fatalf("round %d did not replace the database: %+v", i, res)
		}
		if (res.Backup == nil) != (i == 0) {
			t.Fatalf("round %d kept %v, which is not what there was to keep", i, res.Backup)
		}
	}

	backups := h.backups()
	if len(backups) != 2 {
		t.Fatalf("%d copies kept, want the 2 the setting allows", len(backups))
	}
	// Newest first, and the newest holds what the last round replaced - the
	// database of the round before it.
	got, err := os.ReadFile(backups[0].Path)
	if err != nil {
		t.Fatalf("read the kept copy: %v", err)
	}
	if want := bodies[len(bodies)-2]; string(got) != string(want) {
		t.Errorf("the newest copy holds %d bytes, want the %d of the database it replaced", len(got), len(want))
	}
	// The pruned one is gone from the disk as well as from the table.
	dir := h.engine.backupDir(h.pair)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) != 2 {
		t.Errorf("%d file(s) in %s, want 2", len(entries), dir)
	}
}

// The recovery of S5: a database that came down badly is a move back, not a
// re-transfer of anything.
func TestJCCRollbackRestoresTheKeptCopy(t *testing.T) {
	h := newJCC(t)

	first := h.write(testDB, 1024)
	h.scan()
	h.syncDB(false)

	h.write(testDB, 2048)
	h.scan()
	h.syncDB(false)

	res, err := h.engine.RollbackDatabase(context.Background(), h.pair, 0, false)
	if err != nil {
		t.Fatalf("RollbackDatabase: %v", err)
	}
	if string(h.dbHere()) != string(first) {
		t.Errorf("the database was not put back: %d bytes, want %d", len(h.dbHere()), len(first))
	}
	if res.Replaced == nil {
		t.Error("what the rollback displaced was not itself kept, so the rollback cannot be undone")
	}
	h.wantNoIncoming()

	// Local truth follows the file, so the next walk sees the publisher's copy as
	// something to fetch again - which is the point, when the transfer was the
	// problem.
	file, ok, err := h.store.FileAt(context.Background(), h.pair.ID, testDB)
	if err != nil || !ok {
		t.Fatalf("no row of local truth after the rollback: %v", err)
	}
	if file.Size != int64(len(first)) {
		t.Errorf("local truth says %d bytes, want %d", file.Size, len(first))
	}
}

// A backup that cannot be vouched for is not a backup, and restoring one over a
// working database would be the worst outcome a recovery could have.
func TestJCCRollbackRefusesACopyThatDoesNotMatchItsHash(t *testing.T) {
	h := newJCC(t)

	h.write(testDB, 1024)
	h.scan()
	h.syncDB(false)

	current := h.write(testDB, 2048)
	h.scan()
	h.syncDB(false)

	backups := h.backups()
	if len(backups) != 1 {
		t.Fatalf("%d copies kept, want 1", len(backups))
	}
	if err := os.WriteFile(backups[0].Path, []byte("rot"), 0o644); err != nil {
		t.Fatalf("corrupt the copy: %v", err)
	}

	if _, err := h.engine.RollbackDatabase(context.Background(), h.pair, backups[0].ID, false); err == nil {
		t.Fatal("a corrupted copy was restored")
	}
	if string(h.dbHere()) != string(current) {
		t.Error("the database in place was touched by a rollback that refused")
	}
}

// A lock that has not moved for longer than the threshold is reported rather
// than stolen: jClipCorn deletes its lock on a clean shutdown, so nothing will
// ever clear this one on its own (DESIGN.md §1).
func TestJCCReportsAStaleLock(t *testing.T) {
	h := newJCC(t, func(o *Options) { o.JCC.LockStale = time.Hour })
	h.write(testDB, 1024)
	h.lockRemote()

	long := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(h.src, testLock), long, long); err != nil {
		t.Fatalf("age the lock: %v", err)
	}
	h.scan()

	st, err := h.engine.DatabaseStatus(context.Background(), h.pair)
	if err != nil {
		t.Fatalf("DatabaseStatus: %v", err)
	}
	held := st.Held()
	if len(held) != 1 || held[0].Side != SidePublisher {
		t.Fatalf("held locks = %+v, want the publisher's", held)
	}
	if !held[0].Stale {
		t.Errorf("a lock %s old is not reported as stale", time.Since(held[0].MTime))
	}

	res := h.syncDB(false)
	if res.Replaced || res.Deferred == "" {
		t.Errorf("a stale lock was stolen rather than reported: %+v", res)
	}
	events, err := h.store.Events(context.Background(), store.EventFilter{
		Kinds: []string{store.KindLockStale}, Limit: 5,
	})
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("%d stale-lock event(s), want exactly 1", len(events))
	}

	// And a second cycle against the same lock says nothing new: a scheduled sync
	// comes back every few minutes, and one warning per attempt is a log nobody
	// reads (DESIGN.md §4.1).
	h.syncDB(false)
	events, err = h.store.Events(context.Background(), store.EventFilter{
		Kinds: []string{store.KindLockStale}, Limit: 5,
	})
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("%d stale-lock event(s) after a second identical cycle, want still 1", len(events))
	}

	// The override is the only thing that gets past a stale lock, and it is a
	// person's to give: nothing clears one left by a jClipCorn that crashed.
	if forced := h.syncDB(true); !forced.Replaced || !forced.Forced {
		t.Errorf("the override did not copy the database: %+v", forced)
	}
}

// A mirror pair quarantines what the publisher dropped - except the database,
// which is worth more than one walk's opinion of it.
func TestJCCNeverQuarantinesTheDatabase(t *testing.T) {
	h := newJCC(t)
	h.pair.Mode = store.ModeMirror
	if err := h.store.UpdatePair(context.Background(), &h.pair); err != nil {
		t.Fatalf("make the pair a mirror: %v", err)
	}

	h.write(testDB, 1024)
	h.write("cover/a.jpg", 64)
	h.scan()
	h.sync()
	h.syncDB(false)

	if err := os.Remove(filepath.Join(h.src, testDB)); err != nil {
		t.Fatalf("remove the publisher's database: %v", err)
	}
	h.scan()

	plan, err := h.engine.Plan(context.Background(), h.pair, 10)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Vanished != 0 {
		t.Errorf("the plan wants to delete %d file(s); the database is not one to delete", plan.Vanished)
	}

	res, err := h.engine.Reap(context.Background(), h.pair)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if res.Deleted != 0 {
		t.Errorf("%d file(s) quarantined, want none", res.Deleted)
	}
	if h.dbHere() == nil {
		t.Error("the database here was quarantined")
	}
}

// The gate is an existence check, so a remote that cannot answer one has to say
// so rather than be assumed to mean "nobody is using it".
func TestJCCGateNeedsAProbe(t *testing.T) {
	h := newJCC(t)
	h.engine.remote = noProbe{RangeReader: h.engine.remote}

	if _, err := h.engine.DatabaseStatus(context.Background(), h.pair); err == nil {
		t.Fatal("a remote that cannot probe was accepted by the gate")
	}
}

// noProbe hides the Exists of whatever it wraps.
type noProbe struct{ remote.RangeReader }

func TestJCCPathsFollowTheConfiguredName(t *testing.T) {
	j := JCC{DBDir: "/ClipCornDB/", DBName: "Sammlung"}.fill()

	if got, want := j.DB(), "ClipCornDB/Sammlung.db"; got != want {
		t.Errorf("DB() = %q, want %q", got, want)
	}
	if got, want := j.Lock(), "ClipCornDB/Sammlung.db.~lock"; got != want {
		t.Errorf("Lock() = %q, want %q", got, want)
	}
	if got, want := incomingPath(store.Pair{LocalPath: "/mnt/jcc"}, j.DB()),
		filepath.Join("/mnt/jcc", "ClipCornDB", ".Sammlung.db.incoming"); got != want {
		t.Errorf("staging path = %q, want %q", got, want)
	}
	// The refusals do not follow the name: the per-user databases are what they
	// are called whatever dbName says.
	if !slices.Contains([]string{UserDataDB, HistoryDB}, "ClipCornUserData.db") {
		t.Error("the per-user database names moved")
	}
}
