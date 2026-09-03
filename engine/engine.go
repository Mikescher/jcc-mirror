// Package engine is the mirror itself: the walk that builds a manifest of the
// publisher's tree, the diff that turns it into a plan, and the transfer that
// carries it out. Everything it knows lives in sqlite, so a scan, a queue and a
// half-transferred file all survive a restart, a self-update or a closed
// transfer window (DESIGN.md §2.3, §2.4).
//
// It moves bytes and does not know what is in them. The one exception the design
// allows - the lock gate on ClipCornDB.db - is M5 and is not here.
package engine

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/remote"
	"blackforestbytes.com/jcc-mirror/store"
)

// PartDir holds partial downloads. It lives under the pair's local root so the
// rename at the end is same-filesystem and therefore atomic, and it is hidden so
// Jellyfin and ClipCorn do not scan half a file (DESIGN.md §2.4).
const PartDir = ".jccmirror"

// Options are the engine's settings, read from the config table.
type Options struct {
	ScanWorkers    int
	MTimeTolerance time.Duration
	Chunks         int
	ChunkSize      int64
	MaxAttempts    int
	RetryBackoff   time.Duration
	HashAfterCopy  bool
	Reserve        int64 // free space to leave on the destination volume (S4)

	// The deletion guards of DESIGN.md §2.5. DeletePercent is the share of a pair
	// that may vanish before a person has to approve it; TrashRetention is how long
	// a quarantined file is kept before it is really gone. Zero turns the
	// percentage guard off - the per-pair file count still applies.
	DeletePercent  int
	TrashRetention time.Duration

	// Limiter is the bandwidth cap, and is shared rather than owned: the
	// scheduler holds the same one and changes it live at a window boundary. A
	// nil one is no cap at all.
	Limiter *Limiter
}

// OptionsFrom reads the settings out of a config snapshot.
func OptionsFrom(v store.Values) Options {
	return Options{
		ScanWorkers:    v.Int(store.KeyScanWorkers),
		MTimeTolerance: v.Duration(store.KeyMTimeTolerance),
		Chunks:         v.Int(store.KeyTransferChunks),
		ChunkSize:      v.Size(store.KeyChunkSize),
		MaxAttempts:    v.Int(store.KeyMaxAttempts),
		RetryBackoff:   v.Duration(store.KeyRetryBackoff),
		HashAfterCopy:  v.Bool(store.KeyHashAfterCopy),
		Reserve:        v.Size(store.KeyReserve),
		DeletePercent:  v.Int(store.KeyDeletePercent),
		TrashRetention: v.Duration(store.KeyDeleteRetention),
	}
}

func (o *Options) fill() {
	if o.ScanWorkers < 1 {
		o.ScanWorkers = 8
	}
	// Never zero: WebDAV timestamps carry whole seconds, so an exact comparison
	// makes every file look changed on every scan (DESIGN.md §2.3).
	if o.MTimeTolerance < time.Second {
		o.MTimeTolerance = 2 * time.Second
	}
	if o.Chunks < 1 {
		o.Chunks = 1
	}
	if o.ChunkSize < 1 {
		o.ChunkSize = 64 << 20
	}
	if o.MaxAttempts < 1 {
		o.MaxAttempts = 5
	}
	if o.RetryBackoff <= 0 {
		o.RetryBackoff = 30 * time.Second
	}
	// Never zero: a quarantine with no retention is deletion with extra steps, and
	// the one thing §2.5 will not have is a delete that cannot be undone.
	if o.TrashRetention <= 0 {
		o.TrashRetention = 7 * 24 * time.Hour
	}
}

// Engine runs one pair at a time against one remote.
type Engine struct {
	store  *store.Store
	remote remote.RangeReader
	log    *logs.Logger
	opts   Options

	mu   sync.Mutex
	prog Progress

	runBytes atomic.Int64
	fileDone atomic.Int64
}

// New builds an engine. The remote has to be able to serve bounded ranges:
// resume and per-file parallelism are both built on it, and a remote without it
// would silently transfer the whole file once per stream.
func New(st *store.Store, rem remote.Remote, log *logs.Logger, opts Options) (*Engine, error) {
	rr, ok := rem.(remote.RangeReader)
	if !ok {
		return nil, fmt.Errorf("remote %T cannot serve ranged reads, which is what resume is built on", rem)
	}
	opts.fill()
	return &Engine{store: st, remote: rr, log: log, opts: opts}, nil
}

// Options returns the settings in force, with the defaults filled in.
func (e *Engine) Options() Options { return e.opts }

// Progress is the live state of a run: what the "Now" view shows in M6, and what
// the CLI prints while a sync is going.
type Progress struct {
	Pair       string    `json:"pair,omitempty"`
	Path       string    `json:"path,omitempty"` // the file being transferred right now
	FileDone   int64     `json:"fileDone"`
	FileTotal  int64     `json:"fileTotal"`
	RunBytes   int64     `json:"runBytes"`
	RunTotal   int64     `json:"runTotal"` // what the queue held when the run started
	Files      int       `json:"files"`
	FilesTotal int       `json:"filesTotal"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
}

// Progress reports what the engine is doing right now.
func (e *Engine) Progress() Progress {
	e.mu.Lock()
	p := e.prog
	e.mu.Unlock()

	p.RunBytes = e.runBytes.Load()
	p.FileDone = e.fileDone.Load()
	return p
}

func (e *Engine) beginRun(pair store.Pair, files int, bytes int64) {
	e.runBytes.Store(0)
	e.fileDone.Store(0)

	e.mu.Lock()
	defer e.mu.Unlock()
	e.prog = Progress{Pair: pair.Name, FilesTotal: files, RunTotal: bytes, StartedAt: time.Now()}
}

func (e *Engine) beginFile(relpath string, total int64) {
	e.fileDone.Store(0)

	e.mu.Lock()
	defer e.mu.Unlock()
	e.prog.Path, e.prog.FileTotal = relpath, total
}

func (e *Engine) endFile() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.prog.Files++
	e.prog.Path, e.prog.FileTotal = "", 0
}

func (e *Engine) endRun() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.prog = Progress{}
}

// event records one line of the durable log. Like everywhere else in this
// project a failure to record is logged and swallowed: telemetry must never fail
// the operation that produced it.
func (e *Engine) event(ctx context.Context, level, kind string, pairID int64, message string, data map[string]any) {
	ev := store.Event{Level: level, Kind: kind, Message: message, Data: data}
	if pairID != 0 {
		id := pairID
		ev.PairID = &id
	}
	if err := e.store.AppendEvent(ctx, &ev); err != nil {
		e.log.Errorf("events: %v", err)
	}
}

func (e *Engine) change(ctx context.Context, c *store.Change) {
	if err := e.store.AppendChange(ctx, c); err != nil {
		e.log.Errorf("changes: %v", err)
	}
}

// remotePath is where a pair-relative path lives on the publisher's share.
func remotePath(p store.Pair, rel string) string {
	if p.RemotePath == "" {
		return rel
	}
	if rel == "" {
		return p.RemotePath
	}
	return path.Join(p.RemotePath, rel)
}

// localPath is where a pair-relative path lands here.
func localPath(p store.Pair, rel string) string {
	return filepath.Join(p.LocalPath, filepath.FromSlash(rel))
}

// Transferable refuses to move bytes for a pair whose type needs machinery that
// is not built yet. A jcc pair carries hard exclusions and a lock gate
// (DESIGN.md §3) that arrive in M5, and without them a sync of the ClipCornDB
// directory would overwrite the subscriber's own ClipCornUserData.db and
// ClipCornHistory.db - both per-user, and neither ever synced - and could copy
// the shared database out from under a running jClipCorn. Scanning one is
// harmless and stays allowed; transferring it is not.
func Transferable(p store.Pair) error {
	if p.Type == store.PairJCC {
		return fmt.Errorf("pair %q is a jcc pair: its hard exclusions and lock gate are M5, and transferring one without them would overwrite this side's own ClipCornUserData.db and ClipCornHistory.db", p.Name)
	}
	return nil
}

// errNoManifest is what every operation that reads the manifest returns before
// the first walk has finished. It is deliberately not "nothing to do": with an
// empty manifest a mirror pair would conclude that the publisher deleted
// everything (DESIGN.md §2.5).
var errNoManifest = errors.New("no completed scan yet - run a scan first")
