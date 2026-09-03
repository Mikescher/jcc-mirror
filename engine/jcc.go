package engine

// The jCC pair is an ordinary directory sync plus two rules (DESIGN.md §3).
//
// The first is a set of hard exclusions - refusals rather than user-editable
// patterns, because no configuration makes them correct. The second is a lock
// gate on the shared database alone: it is copied only while no .~lock exists
// beside it on either side, and the lock is re-checked after the copy, because
// user 1 can launch jClipCorn while it runs. The rest of the pair - the covers,
// the program directory - transfers regardless.
//
// jcc-mirror never opens the database and never reads a lock file's contents:
// the body of a .~lock is a PID written by another machine, which is
// uninterpretable here. Its existence is the whole signal.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/remote"
	"blackforestbytes.com/jcc-mirror/store"
)

// The two databases beside the shared one, which are never synced whatever the
// configuration says. Both are per-user: overwriting them here destroys user 2's
// own ratings, tags, filters and history. The history one is the easy one to
// miss - there are three databases in that directory, not two.
const (
	UserDataDB = "ClipCornUserData.db"
	HistoryDB  = "ClipCornHistory.db"
)

// LockSuffix is what jClipCorn's FileLockManager appends: the lock beside
// ClipCornDB.db is ClipCornDB.db.~lock, not ClipCornDB.lock.
const LockSuffix = ".~lock"

// incomingSuffix names the database while it downloads. It is staged in the
// target directory rather than under PartDir so the rename that finishes it is
// same-filesystem and atomic: the local exposure is one syscall rather than the
// duration of a copy (DESIGN.md §3).
const incomingSuffix = ".incoming"

// dbBackupDir is where the kept copies live, under the data volume.
const dbBackupDir = "db-backups"

// backupStamp names one copy. Seconds are enough to order them and the table
// carries the exact time anyway; the collision loop covers the rest.
const backupStamp = "20060102-150405"

// The sides a lock can be held on, as the gate reports them.
const (
	SidePublisher = "publisher"
	SideHere      = "here"
)

// JCC is the jcc pair's half of the configuration (DESIGN.md §6). The database's
// name is jClipCorn's dbName and therefore an install's decision, not something
// to hardcode.
type JCC struct {
	DBDir     string        // where the databases sit inside the pair; "" is the pair root
	DBName    string        // "ClipCornDB", so the shared database is ClipCornDB.db
	LockStale time.Duration // how long a lock may sit unchanged before it is reported
	Backups   int           // copies of the previous database to keep
}

func (j JCC) fill() JCC {
	j.DBDir = strings.Trim(strings.TrimSpace(j.DBDir), "/")
	if j.DBDir != "" {
		j.DBDir = strings.TrimPrefix(path.Clean("/"+j.DBDir), "/")
	}
	if strings.TrimSpace(j.DBName) == "" {
		j.DBName = "ClipCornDB"
	}
	if j.LockStale <= 0 {
		j.LockStale = 24 * time.Hour
	}
	if j.Backups < 1 {
		j.Backups = 5
	}
	return j
}

// DB is the shared database, relative to the pair's root.
func (j JCC) DB() string { return path.Join(j.DBDir, j.DBName+".db") }

// Lock is the lock file that gates it.
func (j JCC) Lock() string { return j.DB() + LockSuffix }

// refuses reports why a path is never transferred, or "" when it may be. These
// are the hard exclusions of DESIGN.md §3, and they are refusals rather than
// patterns an operator can edit because no configuration makes them correct.
func (j JCC) refuses(rel string) string {
	base := path.Base(rel)
	switch {
	case base == UserDataDB || base == HistoryDB:
		return "it is per-user: copying the publisher's over would destroy this side's own ratings, tags and history"
	case strings.HasSuffix(base, LockSuffix):
		return "a lock file belongs to whoever is running"
	case strings.HasSuffix(base, "-journal"), strings.HasSuffix(base, "-wal"), strings.HasSuffix(base, "-shm"):
		return "it is transient and worse than useless apart from the exact database it belongs to"
	case strings.HasPrefix(base, ".") && strings.HasSuffix(base, incomingSuffix):
		return "it is a database of ours that is still being copied"
	}
	return ""
}

// gated is the database the lock gate owns, relative to the pair's root, or "" for
// a pair that has none. It is the one file of a jcc pair that never becomes a
// job: it is transferred by SyncDatabase, between the lock probes.
func (e *Engine) gated(p store.Pair) string {
	if p.Type != store.PairJCC {
		return ""
	}
	return e.opts.JCC.DB()
}

// LockState is one probe of one lock file.
type LockState struct {
	Side  string        `json:"side"` // SidePublisher or SideHere
	Path  string        `json:"path"`
	Held  bool          `json:"held"`
	MTime time.Time     `json:"mtime,omitempty"`
	Age   time.Duration `json:"age,omitempty"`
	// Stale is a lock that has not moved for longer than the threshold. jClipCorn
	// deletes its lock on a clean shutdown, so an old one is a crash nobody
	// restarted - and it is never stolen automatically, only reported
	// (DESIGN.md §1).
	Stale bool `json:"stale,omitempty"`
}

func (l LockState) String() string {
	where := "the publisher's"
	if l.Side == SideHere {
		where = "this side's"
	}
	if l.Stale {
		return fmt.Sprintf("%s lock has not moved for %s", where, format.Duration(l.Age))
	}
	return where + " lock is held"
}

// DBFile is one side's copy of the database.
type DBFile struct {
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
}

// DBStatus is what both sides hold and what stands between them right now. It
// costs four requests over the tunnel, so it is the answer to a question someone
// asked rather than something a page load recomputes.
type DBStatus struct {
	PairID   int64  `json:"pairId"`
	PairName string `json:"pair"`
	Path     string `json:"path"` // the database, relative to the pair's root

	Remote *DBFile     `json:"remote,omitempty"` // nil when the publisher has none
	Local  *DBFile     `json:"local,omitempty"`  // nil when there is none here yet
	Locks  []LockState `json:"locks"`

	InSync  bool             `json:"inSync"`
	Backups []store.DBBackup `json:"backups,omitempty"`
}

// Held returns the locks that stand in the way.
func (s DBStatus) Held() []LockState { return heldLocks(s.Locks) }

// DBResult is what one run of the gate did, or why it did nothing.
type DBResult struct {
	PairID   int64  `json:"pairId"`
	PairName string `json:"pair"`
	Path     string `json:"path"`

	Replaced bool            `json:"replaced"`
	Bytes    int64           `json:"bytes"`
	MTime    time.Time       `json:"mtime,omitempty"`
	Backup   *store.DBBackup `json:"backup,omitempty"` // the copy kept of what was here
	Locks    []LockState     `json:"locks,omitempty"`
	Forced   bool            `json:"forced,omitempty"`

	// Deferred is a cycle that did nothing because a lock was held - by the
	// design's own rule, not by a failure. The next cycle tries again.
	Deferred string `json:"deferred,omitempty"`
	// Skipped is nothing to do at all: no database over there, or the same one here.
	Skipped  string        `json:"skipped,omitempty"`
	Duration time.Duration `json:"duration"`
}

// DatabaseStatus probes both sides: what each holds, which locks are out, and
// what copies are kept here. It is what `jcc-mirror db` prints and what the gate
// itself starts from.
func (e *Engine) DatabaseStatus(ctx context.Context, pair store.Pair) (DBStatus, error) {
	rel := e.gated(pair)
	if rel == "" {
		return DBStatus{}, fmt.Errorf("pair %q is not a jcc pair, so it has no database to gate", pair.Name)
	}

	probe, err := e.prober()
	if err != nil {
		return DBStatus{}, err
	}

	st := DBStatus{PairID: pair.ID, PairName: pair.Name, Path: rel}

	// The publisher's copy. An existence check first, so "he has none" is an
	// answer rather than an error to be guessed at from a failed PROPFIND.
	there, err := probe.Exists(ctx, remotePath(pair, rel))
	if err != nil {
		return DBStatus{}, fmt.Errorf("look for %q on the publisher: %w", rel, err)
	}
	if there {
		entry, err := e.remote.Stat(ctx, remotePath(pair, rel))
		if err != nil {
			return DBStatus{}, fmt.Errorf("read %q on the publisher: %w", rel, err)
		}
		st.Remote = &DBFile{Size: entry.Size, MTime: entry.MTime}
	}

	if info, err := os.Stat(localPath(pair, rel)); err == nil && info.Mode().IsRegular() {
		st.Local = &DBFile{Size: info.Size(), MTime: info.ModTime()}
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return DBStatus{}, fmt.Errorf("read %q here: %w", rel, err)
	}

	if st.Locks, err = e.probeLocks(ctx, pair, probe); err != nil {
		return DBStatus{}, err
	}

	st.InSync = st.Remote != nil && st.Local != nil &&
		st.Remote.Size == st.Local.Size && within(st.Remote.MTime, st.Local.MTime, e.opts.MTimeTolerance)

	if st.Backups, err = e.store.DBBackups(ctx, pair.ID, 0); err != nil {
		return DBStatus{}, err
	}
	return st, nil
}

// SyncDatabase is the gate of DESIGN.md §3, in the order it is written there: the
// two locks, the copy into a staging file beside the target, the size check, the
// same two locks again, the backup, and the rename.
//
// force is the manual override for a lock that has gone stale - a jClipCorn that
// crashed and was never restarted leaves one behind forever, and the database
// would otherwise silently never sync again. It is never taken automatically.
func (e *Engine) SyncDatabase(ctx context.Context, pair store.Pair, force bool) (res DBResult, err error) {
	started := time.Now()
	res = DBResult{PairID: pair.ID, PairName: pair.Name, Forced: force}
	defer func() { res.Duration = time.Since(started) }()

	if pair.Type != store.PairJCC {
		res.Skipped = "the pair is not a jcc pair, so there is no database to gate"
		return res, nil
	}

	st, err := e.DatabaseStatus(ctx, pair)
	if err != nil {
		return res, err
	}
	res.Path, res.Locks = st.Path, st.Locks

	if st.Remote == nil {
		res.Skipped = "the publisher has no " + st.Path
		return res, nil
	}
	if st.InSync {
		// The bytes agree, so nothing moves - but local truth may not know that
		// yet: a database that arrived with the USB bootstrap has no row at all,
		// and without one the diff queues it on every walk.
		if err := e.recordFile(ctx, pair, st.Path, st.Remote.Size, st.Remote.MTime, "", store.FileAdopted); err != nil {
			return res, err
		}
		res.Skipped = "the database here is already the publisher's copy"
		return res, nil
	}

	// Steps 1 and 2. Nothing is fetched while either side has the database open:
	// no lock means a clean shutdown, which means no hot journal, which is what
	// makes a plain byte copy of the .db file consistent (DESIGN.md §1).
	if e.gateHolds(ctx, pair, st.Locks, force, &res) {
		return res, nil
	}

	if err := e.checkSpace(pair, st.Remote.Size); err != nil {
		return res, err
	}

	// Step 3.
	incoming := incomingPath(pair, st.Path)
	discard := func() {
		if rmErr := os.Remove(incoming); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			e.log.Errorf("jcc: %v", rmErr)
		}
	}

	e.log.Infof("jcc: fetching %s of %q (%s)", st.Path, pair.Name, format.Bytes(st.Remote.Size))
	n, err := e.fetchDatabase(ctx, pair, st.Path, st.Remote.Size, incoming)
	if err != nil {
		discard()
		return res, err
	}

	// Step 4. The database is the one file here where a short copy would be
	// silently accepted by everything downstream, so it is checked against the
	// size the PROPFIND reported rather than against the read alone.
	if n != st.Remote.Size {
		discard()
		return res, fmt.Errorf("short copy of %s: %d bytes of %d", st.Path, n, st.Remote.Size)
	}

	// Steps 5 and 6. One extra request each side, and it closes the window in
	// which user 1 launched jClipCorn while the copy was running (S1).
	probe, err := e.prober()
	if err != nil {
		discard()
		return res, err
	}
	after, err := e.probeLocks(ctx, pair, probe)
	if err != nil {
		discard()
		return res, err
	}
	res.Locks = after
	if held := heldLocks(after); len(held) > 0 && !force {
		discard()
		res.Deferred = lockReason(held) + " while the database was being copied, so what arrived is discarded"
		e.noteDeferred(ctx, pair, held, res.Deferred)
		return res, nil
	}

	// The publisher's timestamp, before the rename and not after: without it the
	// local mtime is the copy time and the next walk sees a database that changed
	// (DESIGN.md §2.3).
	if err := os.Chtimes(incoming, st.Remote.MTime, st.Remote.MTime); err != nil {
		discard()
		return res, fmt.Errorf("set the timestamp on %s: %w", st.Path, err)
	}

	// Step 7. A copy rather than a rename, so the database that is there stays
	// there until the one syscall that replaces it (S5).
	backup, err := e.backupDatabase(ctx, pair, st.Path, store.BackupReplace)
	if err != nil {
		discard()
		return res, err
	}
	res.Backup = backup

	// Step 8.
	if err := os.Rename(incoming, localPath(pair, st.Path)); err != nil {
		discard()
		return res, fmt.Errorf("move %s into place: %w", st.Path, err)
	}

	if err := e.recordFile(ctx, pair, st.Path, st.Remote.Size, st.Remote.MTime, "", store.FileSynced); err != nil {
		return res, err
	}
	res.Replaced, res.Bytes, res.MTime = true, st.Remote.Size, st.Remote.MTime

	e.pruneBackups(ctx, pair)

	msg := fmt.Sprintf("%s of %q replaced: %s, dated %s",
		st.Path, pair.Name, format.Bytes(res.Bytes), res.MTime.Local().Format("2006-01-02 15:04:05"))
	if force {
		msg += " (a held lock was overridden)"
	}
	e.log.Infof("jcc: %s", msg)
	e.noteDB(ctx, pair, store.LevelInfo, store.KindDBReplaced, msg, map[string]any{
		"path": st.Path, "bytes": res.Bytes, "forced": force, "backup": backupPath(backup),
	})
	return res, nil
}

// gateHolds answers steps 1 and 2, and reports whether the run stops here.
func (e *Engine) gateHolds(ctx context.Context, pair store.Pair, locks []LockState, force bool, res *DBResult) bool {
	held := heldLocks(locks)
	if len(held) == 0 {
		return false
	}
	if force {
		e.log.Warnf("jcc: %s, and the override was given - copying %s anyway", lockReason(held), res.Path)
		return false
	}

	res.Deferred = lockReason(held) + ", so the database is left alone this cycle"
	e.noteDeferred(ctx, pair, held, res.Deferred)
	return true
}

// noteDeferred records a skipped cycle. A stale lock is the interesting one and
// is reported as such: it will not clear on its own, and without the override
// the database silently never syncs again (DESIGN.md §1).
func (e *Engine) noteDeferred(ctx context.Context, pair store.Pair, held []LockState, reason string) {
	level, kind := store.LevelInfo, store.KindDBSkipped
	msg := fmt.Sprintf("%s of %q was not copied: %s", e.opts.JCC.DB(), pair.Name, reason)

	for _, l := range held {
		if l.Stale {
			level, kind = store.LevelWarn, store.KindLockStale
			msg = fmt.Sprintf("%s of %q has not synced: %s, longer than the %s that counts as stale. jClipCorn deletes its lock on a clean shutdown, so this is a crash nobody restarted - and it is never stolen automatically.",
				e.opts.JCC.DB(), pair.Name, l.String(), format.Duration(e.opts.JCC.LockStale))
			break
		}
	}

	e.log.Infof("jcc: %s", msg)
	e.noteDB(ctx, pair, level, kind, msg, map[string]any{"path": e.opts.JCC.DB(), "locks": held})
}

// fetchDatabase copies the whole file in one stream. There is no resume and no
// chunking here on purpose: it is a few MB, and a database is only worth anything
// end to end - half of yesterday's and half of today's is not a database.
func (e *Engine) fetchDatabase(ctx context.Context, pair store.Pair, rel string, size int64, dst string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, fmt.Errorf("create %q: %w", filepath.Dir(dst), err)
	}

	f, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("open the staging file: %w", err)
	}
	defer f.Close()

	body, _, err := e.remote.OpenRange(ctx, remotePath(pair, rel), 0, 0)
	if err != nil {
		return 0, err
	}
	defer body.Close()

	e.beginFile(rel, size)
	defer e.endFile()

	// Through the shared limiter like any other transfer: the cap is there to
	// protect the link, and the link does not care which file it is.
	n, err := io.Copy(f, &countingReader{r: body, ctx: ctx, engine: e})
	if err != nil {
		return n, fmt.Errorf("copy %s: %w", rel, err)
	}
	if err := f.Sync(); err != nil {
		return n, fmt.Errorf("flush the staging file: %w", err)
	}
	return n, nil
}

// backupDatabase keeps a copy of what is in place before it is replaced, and
// returns nil when there is nothing there yet. The copy carries the mtime of the
// original, so a rollback restores the publisher's timestamp along with the bytes
// rather than making the next walk see a database that changed.
func (e *Engine) backupDatabase(ctx context.Context, pair store.Pair, rel, reason string) (*store.DBBackup, error) {
	src := localPath(pair, rel)
	info, err := os.Stat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %q before replacing it: %w", rel, err)
	}

	dir := e.backupDir(pair)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create the backup directory: %w", err)
	}
	dst, err := backupName(dir, path.Base(rel), time.Now())
	if err != nil {
		return nil, err
	}

	size, hash, err := copyFile(src, dst)
	if err != nil {
		return nil, err
	}
	if err := os.Chtimes(dst, info.ModTime(), info.ModTime()); err != nil {
		return nil, fmt.Errorf("stamp the backup: %w", err)
	}

	b := &store.DBBackup{
		PairID: pair.ID, RelDB: rel, Path: dst, Size: size,
		MTime: info.ModTime(), Hash: hash, Reason: reason,
	}
	if err := e.store.AddDBBackup(ctx, b); err != nil {
		return nil, err
	}
	e.log.Infof("jcc: kept %s as %s", rel, dst)
	return b, nil
}

// pruneBackups keeps the newest few and lets the rest go. A failure is logged and
// swallowed: a gate that cannot let go of an old copy is still a gate that
// replaced the database correctly.
func (e *Engine) pruneBackups(ctx context.Context, pair store.Pair) {
	excess, err := e.store.ExcessDBBackups(ctx, pair.ID, e.opts.JCC.Backups)
	if err != nil {
		e.log.Errorf("jcc: %v", err)
		return
	}
	for _, b := range excess {
		if err := os.Remove(b.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			e.log.Errorf("jcc: %v", err)
			continue
		}
		if err := e.store.DeleteDBBackup(ctx, b.ID); err != nil {
			e.log.Errorf("jcc: %v", err)
		}
	}
}

// RollbackResult is one database put back.
type RollbackResult struct {
	PairID   int64           `json:"pairId"`
	PairName string          `json:"pair"`
	Path     string          `json:"path"`
	Backup   store.DBBackup  `json:"backup"`             // the copy that was restored
	Replaced *store.DBBackup `json:"replaced,omitempty"` // and the copy taken of what it displaced
	Duration time.Duration   `json:"duration"`
}

// RollbackDatabase puts a kept copy back. It is the recovery story for a bad
// transfer, which - once the lock gate has ruled out a torn database - is the
// only realistic failure left (DESIGN.md S5).
//
// It touches nothing on the publisher and asks him nothing, so it works with the
// tunnel down. It does check this side's lock: replacing the database under a
// running jClipCorn is the one way a rollback could make things worse. What it
// displaces is itself kept, so a rollback is undoable.
func (e *Engine) RollbackDatabase(ctx context.Context, pair store.Pair, backupID int64, force bool) (res RollbackResult, err error) {
	started := time.Now()
	defer func() { res.Duration = time.Since(started) }()

	rel := e.gated(pair)
	if rel == "" {
		return res, fmt.Errorf("pair %q is not a jcc pair, so it has no database to roll back", pair.Name)
	}
	res = RollbackResult{PairID: pair.ID, PairName: pair.Name, Path: rel}

	backup, err := e.pickBackup(ctx, pair, backupID)
	if err != nil {
		return res, err
	}
	res.Backup = backup

	lock, err := e.localLock(pair, e.opts.JCC.Lock())
	if err != nil {
		return res, err
	}
	if lock.Held && !force {
		return res, fmt.Errorf("%s: close jClipCorn here first, or override it", lock.String())
	}

	// Verified before anything is moved. A backup whose bytes cannot be vouched
	// for is not a backup, and restoring one over a working database would be the
	// worst possible outcome of a recovery.
	if err := verifyBackup(backup); err != nil {
		return res, err
	}

	incoming := incomingPath(pair, rel)
	if _, _, err := copyFile(backup.Path, incoming); err != nil {
		return res, err
	}
	discard := func() {
		if rmErr := os.Remove(incoming); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			e.log.Errorf("jcc: %v", rmErr)
		}
	}
	if err := os.Chtimes(incoming, backup.MTime, backup.MTime); err != nil {
		discard()
		return res, fmt.Errorf("set the timestamp on %s: %w", rel, err)
	}

	if res.Replaced, err = e.backupDatabase(ctx, pair, rel, store.BackupRollback); err != nil {
		discard()
		return res, err
	}
	if err := os.Rename(incoming, localPath(pair, rel)); err != nil {
		discard()
		return res, fmt.Errorf("move %s into place: %w", rel, err)
	}

	if err := e.recordFile(ctx, pair, rel, backup.Size, backup.MTime, backup.Hash, store.FileSynced); err != nil {
		return res, err
	}

	msg := fmt.Sprintf("%s of %q rolled back to the copy of %s (%s)",
		rel, pair.Name, backup.TS.Local().Format("2006-01-02 15:04:05"), format.Bytes(backup.Size))
	e.log.Infof("jcc: %s", msg)
	e.noteDB(ctx, pair, store.LevelWarn, store.KindDBRollback, msg, map[string]any{
		"path": rel, "backupId": backup.ID, "bytes": backup.Size, "forced": force,
	})
	return res, nil
}

// pickBackup resolves what an operator named: an id, or the newest copy.
func (e *Engine) pickBackup(ctx context.Context, pair store.Pair, id int64) (store.DBBackup, error) {
	if id > 0 {
		b, err := e.store.DBBackupByID(ctx, id)
		if err != nil {
			return store.DBBackup{}, err
		}
		if b.PairID != pair.ID {
			return store.DBBackup{}, fmt.Errorf("backup %d belongs to another pair", id)
		}
		return b, nil
	}

	all, err := e.store.DBBackups(ctx, pair.ID, 1)
	if err != nil {
		return store.DBBackup{}, err
	}
	if len(all) == 0 {
		return store.DBBackup{}, fmt.Errorf("%w for %q: nothing has replaced its database yet", store.ErrNoBackup, pair.Name)
	}
	return all[0], nil
}

// verifyBackup checks a copy against what was recorded of it.
func verifyBackup(b store.DBBackup) error {
	info, err := os.Stat(b.Path)
	if err != nil {
		return fmt.Errorf("read the backup: %w", err)
	}
	if info.Size() != b.Size {
		return fmt.Errorf("the backup at %s is %d bytes, not the %d that were recorded; it will not be restored", b.Path, info.Size(), b.Size)
	}

	f, err := os.Open(b.Path)
	if err != nil {
		return fmt.Errorf("read the backup: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("read the backup: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != b.Hash {
		return fmt.Errorf("the backup at %s does not match its recorded hash; it will not be restored", b.Path)
	}
	return nil
}

// probeLocks answers "is either side using the database", in the order §3 asks:
// the publisher first, because that is the one an extra request has to reach.
func (e *Engine) probeLocks(ctx context.Context, pair store.Pair, probe remote.Prober) ([]LockState, error) {
	rel := e.opts.JCC.Lock()

	there, err := e.remoteLock(ctx, pair, probe, rel)
	if err != nil {
		return nil, err
	}
	here, err := e.localLock(pair, rel)
	if err != nil {
		return nil, err
	}
	return []LockState{there, here}, nil
}

func (e *Engine) remoteLock(ctx context.Context, pair store.Pair, probe remote.Prober, rel string) (LockState, error) {
	l := LockState{Side: SidePublisher, Path: remotePath(pair, rel)}

	held, err := probe.Exists(ctx, l.Path)
	if err != nil {
		return LockState{}, fmt.Errorf("probe for %q: %w", l.Path, err)
	}
	if !held {
		return l, nil
	}
	l.Held = true

	// The mtime is only wanted for the staleness question, so a server that will
	// not answer it costs the warning, not the gate.
	entry, err := e.remote.Stat(ctx, l.Path)
	if err != nil {
		e.log.Debugf("jcc: %q is there but will not stat: %v", l.Path, err)
		return l, nil
	}
	l.MTime = entry.MTime
	l.Age = time.Since(entry.MTime)
	l.Stale = e.opts.JCC.LockStale > 0 && l.Age > e.opts.JCC.LockStale
	return l, nil
}

func (e *Engine) localLock(pair store.Pair, rel string) (LockState, error) {
	l := LockState{Side: SideHere, Path: localPath(pair, rel)}

	info, err := os.Lstat(l.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return l, nil
	case err != nil:
		// Not "no lock". A directory that cannot be read is a question the gate
		// has no answer to, and the answer it must not assume is the permissive one.
		return LockState{}, fmt.Errorf("probe for %q: %w", l.Path, err)
	}

	l.Held = true
	l.MTime = info.ModTime()
	l.Age = time.Since(info.ModTime())
	l.Stale = e.opts.JCC.LockStale > 0 && l.Age > e.opts.JCC.LockStale
	return l, nil
}

// prober is the existence check the gate is built on. HEAD rather than a listing:
// the lock sits beside a database that may be in a directory of ten thousand
// covers, and its body is a PID from another machine that is never read.
func (e *Engine) prober() (remote.Prober, error) {
	p, ok := e.remote.(remote.Prober)
	if !ok {
		return nil, fmt.Errorf("remote %T cannot probe for a single file, and the lock gate is exactly that", e.remote)
	}
	return p, nil
}

func heldLocks(locks []LockState) []LockState {
	var out []LockState
	for _, l := range locks {
		if l.Held {
			out = append(out, l)
		}
	}
	return out
}

// lockReason says which side is holding, in the words the event log uses.
func lockReason(held []LockState) string {
	parts := make([]string, 0, len(held))
	for _, l := range held {
		parts = append(parts, l.String())
	}
	return strings.Join(parts, " and ")
}

// dbEventKinds is the gate's own line of the event log, whatever it last said.
var dbEventKinds = []string{store.KindDBReplaced, store.KindDBSkipped, store.KindLockStale, store.KindDBRollback}

// noteDB records what the gate did, unless the last thing it recorded said the
// same. A scheduled sync comes back to the same held lock every few minutes, and
// one line per attempt about a state that has not changed is a log nobody reads
// (DESIGN.md §4.1).
func (e *Engine) noteDB(ctx context.Context, pair store.Pair, level, kind, msg string, data map[string]any) {
	id := pair.ID
	last, err := e.store.Events(ctx, store.EventFilter{PairID: &id, Kinds: dbEventKinds, Limit: 1})
	if err != nil {
		e.log.Errorf("events: %v", err)
	} else if len(last) > 0 && last[0].Kind == kind && last[0].Message == msg {
		return
	}
	e.event(ctx, level, kind, pair.ID, msg, data)
}

// backupDir is where a pair's kept copies live. The data volume rather than the
// pair, because the volume is the one thing a rebuilt container keeps
// (DESIGN.md §8); with no data volume configured - the tests, a command run from
// a working directory - they go beside the partials instead.
func (e *Engine) backupDir(pair store.Pair) string {
	root := e.opts.DataDir
	if root == "" {
		root = filepath.Join(pair.LocalPath, PartDir)
	}
	return filepath.Join(root, dbBackupDir, strconv.FormatInt(pair.ID, 10))
}

// backupName picks a free name for a copy. Two replacements in the same second
// are not a thing that happens, and losing one to the other silently would be.
func backupName(dir, base string, at time.Time) (string, error) {
	stem := filepath.Join(dir, base+"."+at.Format(backupStamp))
	for i := 0; i < 100; i++ {
		candidate := stem
		if i > 0 {
			candidate = fmt.Sprintf("%s.%d", stem, i)
		}
		_, err := os.Lstat(candidate)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return candidate, nil
		case err != nil:
			return "", fmt.Errorf("check the backup path: %w", err)
		}
	}
	return "", fmt.Errorf("a hundred backups of %q in one second", base)
}

// incomingPath is where the database is staged, in the directory it will land in
// so the rename is same-filesystem and atomic (DESIGN.md §3).
func incomingPath(pair store.Pair, rel string) string {
	dir, base := path.Split(rel)
	return localPath(pair, path.Join(dir, "."+base+incomingSuffix))
}

// copyFile copies src to dst and returns what it wrote and its sha256. The hash
// is taken from the bytes on their way past rather than by reading the copy back:
// one pass, and it is the copy's own content that is hashed either way.
func copyFile(src, dst string) (int64, string, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, "", fmt.Errorf("read %q: %w", src, err)
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, "", fmt.Errorf("create %q: %w", filepath.Dir(dst), err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, "", fmt.Errorf("write %q: %w", dst, err)
	}
	defer out.Close()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err != nil {
		return n, "", fmt.Errorf("copy %q: %w", src, err)
	}
	if err := out.Sync(); err != nil {
		return n, "", fmt.Errorf("flush %q: %w", dst, err)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func backupPath(b *store.DBBackup) string {
	if b == nil {
		return ""
	}
	return b.Path
}
