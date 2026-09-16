package engine

import (
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/remote"
	"blackforestbytes.com/jcc-mirror/remote/localfs"
	"blackforestbytes.com/jcc-mirror/store"
)

// harness is a whole mirror with no network: a directory standing in for the
// publisher's share, a directory standing in for the Synology, and the sqlite
// state between them. localfs truncates its timestamps to whole seconds, so a
// test cannot pass here on fidelity the real remote does not have.
type harness struct {
	t      *testing.T
	store  *store.Store
	engine *Engine
	pair   store.Pair
	src    string
	dst    string
}

func newHarness(t *testing.T, opts ...func(*Options)) *harness {
	t.Helper()
	ctx := context.Background()

	src, dst := t.TempDir(), t.TempDir()
	st, err := store.OpenFile(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	pair := store.Pair{Name: "media", RemotePath: "", LocalPath: dst, Mode: store.ModeAdditive, Enabled: true}
	if err := st.CreatePair(ctx, &pair); err != nil {
		t.Fatalf("create pair: %v", err)
	}

	o := Options{Chunks: 2, ChunkSize: 8 << 10}
	for _, fn := range opts {
		fn(&o)
	}

	fs, err := localfs.New(src)
	if err != nil {
		t.Fatalf("localfs: %v", err)
	}
	e, err := New(st, fs, &logs.Logger{}, o)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}

	return &harness{t: t, store: st, engine: e, pair: pair, src: src, dst: dst}
}

// write puts a file on the publisher's side and returns its content.
func (h *harness) write(rel string, size int) []byte {
	h.t.Helper()
	body := make([]byte, size)
	if _, err := rand.Read(body); err != nil {
		h.t.Fatalf("rand: %v", err)
	}

	full := filepath.Join(h.src, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		h.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, body, 0o644); err != nil {
		h.t.Fatalf("write %s: %v", rel, err)
	}
	return body
}

func (h *harness) scan() ScanResult {
	h.t.Helper()
	res, err := h.engine.Scan(context.Background(), h.pair, false)
	if err != nil {
		h.t.Fatalf("scan: %v", err)
	}
	return res
}

func (h *harness) sync() SyncResult {
	h.t.Helper()
	if _, err := h.engine.Enqueue(context.Background(), h.pair); err != nil {
		h.t.Fatalf("enqueue: %v", err)
	}
	res, err := h.engine.Sync(context.Background(), h.pair)
	if err != nil {
		h.t.Fatalf("sync: %v", err)
	}
	return res
}

// wantFile checks that a file landed with the right bytes and the publisher's
// timestamp - the second half is what keeps the next scan from transferring it
// all over again.
func (h *harness) wantFile(rel string, want []byte) {
	h.t.Helper()

	got, err := os.ReadFile(filepath.Join(h.dst, filepath.FromSlash(rel)))
	if err != nil {
		h.t.Fatalf("read %s: %v", rel, err)
	}
	if len(got) != len(want) {
		h.t.Fatalf("%s: %d bytes, want %d", rel, len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			h.t.Fatalf("%s: byte %d differs", rel, i)
		}
	}

	src, err := os.Stat(filepath.Join(h.src, filepath.FromSlash(rel)))
	if err != nil {
		h.t.Fatalf("stat source: %v", err)
	}
	dst, err := os.Stat(filepath.Join(h.dst, filepath.FromSlash(rel)))
	if err != nil {
		h.t.Fatalf("stat destination: %v", err)
	}
	if d := dst.ModTime().Sub(src.ModTime().Truncate(time.Second)); d < -time.Second || d > time.Second {
		h.t.Errorf("%s: mtime is %s, want the publisher's %s", rel, dst.ModTime(), src.ModTime())
	}
}

// local puts a file under the destination root - what a USB bootstrap, an
// interrupted run or a crash left there. The timestamp is the caller's: it is
// exactly the thing adoption must not trust (DESIGN.md §2.4).
func (h *harness) local(rel string, body []byte, mtime time.Time) {
	h.t.Helper()

	full := filepath.Join(h.dst, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		h.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, body, 0o644); err != nil {
		h.t.Fatalf("write %s: %v", rel, err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(full, mtime, mtime); err != nil {
			h.t.Fatalf("chtimes %s: %v", rel, err)
		}
	}
}

// addPair registers a second pair, for what one cannot express: another remote
// root, another local root.
func (h *harness) addPair(p store.Pair) store.Pair {
	h.t.Helper()

	if p.LocalPath == "" {
		p.LocalPath = h.t.TempDir()
	}
	p.Enabled = true
	if err := h.store.CreatePair(context.Background(), &p); err != nil {
		h.t.Fatalf("create pair %q: %v", p.Name, err)
	}
	return p
}

// manifest is the last walk as the database holds it, directories included.
func (h *harness) manifest(pair store.Pair) map[string]store.ManifestEntry {
	h.t.Helper()

	rows, err := h.store.DB().QueryContext(context.Background(),
		`SELECT relpath, size, mtime, is_dir FROM manifest WHERE pair_id = ?`, pair.ID)
	if err != nil {
		h.t.Fatalf("read manifest: %v", err)
	}
	defer rows.Close()

	out := map[string]store.ManifestEntry{}
	for rows.Next() {
		var (
			e     store.ManifestEntry
			ms    int64
			isDir int
		)
		if err := rows.Scan(&e.Path, &e.Size, &ms, &isDir); err != nil {
			h.t.Fatalf("scan manifest row: %v", err)
		}
		e.MTime, e.IsDir = time.UnixMilli(ms), isDir != 0
		out[e.Path] = e
	}
	if err := rows.Err(); err != nil {
		h.t.Fatalf("read manifest: %v", err)
	}
	return out
}

// jobs is the pair's whole queue, in the order it runs.
func (h *harness) jobs() []store.Job {
	h.t.Helper()

	jobs, err := h.store.Jobs(context.Background(), store.JobFilter{PairID: &h.pair.ID, Limit: 1000})
	if err != nil {
		h.t.Fatalf("read jobs: %v", err)
	}
	return jobs
}

// wantNoStaging checks that nothing is left in the staging directory: a .part
// that survives a clean run is a file the next one would resume into.
func (h *harness) wantNoStaging() {
	h.t.Helper()

	entries, err := os.ReadDir(filepath.Join(h.dst, PartDir))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		h.t.Fatalf("read %s: %v", PartDir, err)
	}
	if len(entries) != 0 {
		h.t.Errorf("%d partial file(s) left in %s", len(entries), PartDir)
	}
}

// wrap puts a controllable remote in front of the publisher's directory and
// returns it, so a test can make a listing or a ranged read fail without a
// network and without waiting for anything.
func (h *harness) wrap() *fakeRemote {
	h.t.Helper()

	fs, ok := h.engine.remote.(*localfs.FS)
	if !ok {
		h.t.Fatalf("the engine's remote is %T, want the harness's *localfs.FS", h.engine.remote)
	}
	w := &fakeRemote{FS: fs}
	h.engine.remote = w
	return w
}

// fakeRemote is the publisher's share with a fault injector in front of it. Both
// hooks run before the call they belong to; a nil hook lets everything through.
type fakeRemote struct {
	*localfs.FS

	onList  func(dir string) error
	onRange func(path string, off, length int64) error

	mu     sync.Mutex
	ranges []rangeRequest
}

type rangeRequest struct {
	path   string
	off    int64
	length int64
}

func (f *fakeRemote) List(ctx context.Context, dir string) ([]remote.Entry, error) {
	if f.onList != nil {
		if err := f.onList(dir); err != nil {
			return nil, err
		}
	}
	return f.FS.List(ctx, dir)
}

func (f *fakeRemote) OpenRange(ctx context.Context, p string, off, length int64) (io.ReadCloser, int64, error) {
	f.mu.Lock()
	f.ranges = append(f.ranges, rangeRequest{path: p, off: off, length: length})
	f.mu.Unlock()

	if f.onRange != nil {
		if err := f.onRange(p, off, length); err != nil {
			return nil, 0, err
		}
	}
	return f.FS.OpenRange(ctx, p, off, length)
}

// fetched is how many bytes the engine asked the publisher for, which is what a
// resume exists to keep small.
func (f *fakeRemote) fetched() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()

	var n int64
	for _, r := range f.ranges {
		n += r.length
	}
	return n
}

// requests returns the ranges asked for.
func (f *fakeRemote) requests() []rangeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rangeRequest(nil), f.ranges...)
}

// lowestOffset is the earliest byte the engine asked for, or -1 if it asked for
// nothing. The chunks of a round run in parallel, so which one is requested
// first is not something a test may depend on.
func (f *fakeRemote) lowestOffset() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()

	lowest := int64(-1)
	for _, r := range f.ranges {
		if lowest < 0 || r.off < lowest {
			lowest = r.off
		}
	}
	return lowest
}

var _ remote.RangeReader = (*localfs.FS)(nil)
var _ remote.RangeReader = (*fakeRemote)(nil)

// TestSyncTransfersAndIsIdempotent is the whole milestone in one test: walk,
// diff, transfer, and then a second round that finds nothing left to do. The
// second round is the one that matters - it is where a mtime the differ cannot
// reproduce would show up as thirty terabytes transferring twice.
func TestSyncTransfersAndIsIdempotent(t *testing.T) {
	h := newHarness(t)

	want := map[string][]byte{
		"Filme/Grüße.mkv":        h.write("Filme/Grüße.mkv", 40*1024),
		"Filme/A/small.mkv":      h.write("Filme/A/small.mkv", 17),
		"Serien/S01/e01.mkv":     h.write("Serien/S01/e01.mkv", 3),
		"ClipCornDB/cover/a.jpg": h.write("ClipCornDB/cover/a.jpg", 1024),
		"empty.bin":              h.write("empty.bin", 0),
	}

	scan := h.scan()
	if scan.Scan.Files != int64(len(want)) {
		t.Errorf("scan found %d files, want %d", scan.Scan.Files, len(want))
	}

	res := h.sync()
	if res.Files != len(want) {
		t.Errorf("transferred %d files, want %d", res.Files, len(want))
	}
	for rel, body := range want {
		h.wantFile(rel, body)
	}

	// Nothing may be left staged: a .part that survives is a file the next run
	// would resume into.
	if entries, err := os.ReadDir(filepath.Join(h.dst, PartDir)); err == nil && len(entries) != 0 {
		t.Errorf("%d partial file(s) left behind", len(entries))
	}

	h.scan()
	plan, err := h.engine.Plan(context.Background(), h.pair, 10)
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if plan.Transfers() != 0 {
		t.Errorf("second plan wants %d transfers, want none: %+v", plan.Transfers(), plan.Entries)
	}
	if plan.Vanished != 0 {
		t.Errorf("second plan reports %d vanished files, want none", plan.Vanished)
	}
}
