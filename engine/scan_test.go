package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"blackforestbytes.com/jcc-mirror/store"
)

// tree is the shape of the publisher's share in miniature: several levels, two
// files in one directory so a deletion can be told from an empty listing.
var tree = map[string]int{
	"top.mkv":            5,
	"Filme/A/Grüße.mkv":  10,
	"Filme/B/b.mkv":      20,
	"Serien/S01/e01.mkv": 30,
	"Serien/S01/e02.mkv": 40,
}

var treeDirs = []string{"", "Filme", "Filme/A", "Filme/B", "Serien", "Serien/S01"}

func writeTree(h *harness) map[string][]byte {
	h.t.Helper()

	out := make(map[string][]byte, len(tree))
	for rel, size := range tree {
		out[rel] = h.write(rel, size)
	}
	return out
}

func TestScanWalksTheWholeTree(t *testing.T) {
	h := newHarness(t)
	writeTree(h)

	res := h.scan()
	if res.Scan.Files != int64(len(tree)) {
		t.Errorf("scan found %d files, want %d", res.Scan.Files, len(tree))
	}
	if res.Scan.Dirs != int64(len(treeDirs)) {
		t.Errorf("scan listed %d directories, want %d", res.Scan.Dirs, len(treeDirs))
	}
	if res.Scan.State != store.ScanDone {
		t.Errorf("scan state is %q, want %q", res.Scan.State, store.ScanDone)
	}

	m := h.manifest(h.pair)
	if len(m) != len(tree)+len(treeDirs) {
		t.Errorf("manifest has %d rows, want %d files and %d directories", len(m), len(tree), len(treeDirs))
	}
	for rel, size := range tree {
		e, ok := m[rel]
		if !ok {
			t.Errorf("%q is missing from the manifest", rel)
			continue
		}
		if e.IsDir {
			t.Errorf("%q is recorded as a directory", rel)
		}
		if e.Size != int64(size) {
			t.Errorf("%q has size %d in the manifest, want %d", rel, e.Size, size)
		}
		if e.MTime.IsZero() {
			t.Errorf("%q has no mtime in the manifest", rel)
		}
	}
	for _, dir := range treeDirs {
		e, ok := m[dir]
		if !ok {
			t.Errorf("directory %q is missing from the manifest", dir)
			continue
		}
		if !e.IsDir {
			t.Errorf("%q is not recorded as a directory", dir)
		}
	}
}

// TestScanNeverListsAnExcludedSubtree is the cheap half of an exclude: the walk
// costs one PROPFIND per directory against a link with real latency, so an
// excluded subtree must not be walked at all rather than walked and dropped.
func TestScanNeverListsAnExcludedSubtree(t *testing.T) {
	h := newHarness(t)
	writeTree(h)

	pair := h.pair
	pair.Excludes = []string{"Serien", "**/*.nfo"}
	h.write("Filme/A/a.nfo", 3)

	w := h.wrap()
	var (
		mu     sync.Mutex
		listed []string
	)
	w.onList = func(dir string) error {
		mu.Lock()
		defer mu.Unlock()
		listed = append(listed, dir)
		return nil
	}

	res, err := h.engine.Scan(context.Background(), pair, false)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.Scan.Files != 3 {
		t.Errorf("scan found %d files, want the 3 outside the excludes", res.Scan.Files)
	}

	for _, dir := range listed {
		if dir == "Serien" || dir == "Serien/S01" {
			t.Errorf("the walk listed %q, which the pair excludes", dir)
		}
	}
	for path := range h.manifest(h.pair) {
		if path == "Serien" || strings.HasPrefix(path, "Serien/") {
			t.Errorf("the manifest holds %q under an excluded subtree", path)
		}
		if filepath.Ext(path) == ".nfo" {
			t.Errorf("the manifest holds %q, which the pair excludes", path)
		}
	}
}

// TestScanResumesAnInterruptedWalk is the reason the frontier lives in the
// manifest: a walk of the real collection runs for minutes, and a restart or a
// closed window must not send it back to the root (DESIGN.md §2.3).
func TestScanResumesAnInterruptedWalk(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.ScanWorkers = 1 })
	writeTree(h)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := h.wrap()
	var lists atomic.Int64
	w.onList = func(string) error {
		// Stop once the root is listed and its children are on the frontier, which
		// is the state a killed process leaves behind.
		if lists.Add(1) > 1 {
			cancel()
			return ctx.Err()
		}
		return nil
	}

	if _, err := h.engine.Scan(ctx, h.pair, false); err == nil {
		t.Fatal("an interrupted scan reported success")
	}

	first, ok, err := h.store.RunningScan(context.Background(), h.pair.ID)
	if err != nil {
		t.Fatalf("running scan: %v", err)
	}
	if !ok {
		t.Fatal("the interrupted walk left nothing to resume - the next run would start at the root")
	}

	w.onList = nil
	res, err := h.engine.Scan(context.Background(), h.pair, true)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !res.Resumed {
		t.Error("the second scan did not report a resume")
	}
	if res.Scan.ID != first.ID {
		t.Errorf("the resume ran as scan %d, want the interrupted %d", res.Scan.ID, first.ID)
	}
	// Counted once each: a resume that re-listed what was already done would
	// double these, one that skipped a directory would leave them short.
	if res.Scan.Files != int64(len(tree)) {
		t.Errorf("the resumed scan counts %d files, want %d", res.Scan.Files, len(tree))
	}
	if res.Scan.Dirs != int64(len(treeDirs)) {
		t.Errorf("the resumed scan counts %d directories, want %d", res.Scan.Dirs, len(treeDirs))
	}

	m := h.manifest(h.pair)
	for rel := range tree {
		if _, ok := m[rel]; !ok {
			t.Errorf("%q is missing from the manifest after the resume", rel)
		}
	}
}

func TestScanWithoutResumeStartsANewWalk(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.ScanWorkers = 1 })
	writeTree(h)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := h.wrap()
	var lists atomic.Int64
	w.onList = func(string) error {
		if lists.Add(1) > 1 {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	if _, err := h.engine.Scan(ctx, h.pair, false); err == nil {
		t.Fatal("an interrupted scan reported success")
	}
	first, _, err := h.store.RunningScan(context.Background(), h.pair.ID)
	if err != nil {
		t.Fatalf("running scan: %v", err)
	}

	w.onList = nil
	res, err := h.engine.Scan(context.Background(), h.pair, false)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.Resumed {
		t.Error("a scan asked to start over reported a resume")
	}
	if res.Scan.ID == first.ID {
		t.Errorf("the new scan reused row %d instead of starting one", first.ID)
	}
	if res.Scan.Files != int64(len(tree)) {
		t.Errorf("the new scan counts %d files, want %d", res.Scan.Files, len(tree))
	}

	m := h.manifest(h.pair)
	for rel := range tree {
		if _, ok := m[rel]; !ok {
			t.Errorf("%q is missing from the manifest", rel)
		}
	}
}

func TestScanSweepsWhatThePublisherDeleted(t *testing.T) {
	h := newHarness(t)
	writeTree(h)
	h.scan()

	if err := os.Remove(filepath.Join(h.src, filepath.FromSlash("Serien/S01/e02.mkv"))); err != nil {
		t.Fatalf("remove: %v", err)
	}

	res := h.scan()
	if res.Swept != 1 {
		t.Errorf("the scan swept %d manifest rows, want 1", res.Swept)
	}
	if _, ok := h.manifest(h.pair)["Serien/S01/e02.mkv"]; ok {
		t.Error("the deleted file is still in the manifest")
	}
	if res.Scan.Files != int64(len(tree))-1 {
		t.Errorf("the scan counts %d files, want %d", res.Scan.Files, len(tree)-1)
	}
}

// TestScanRefusesAnEmptyRemoteRoot is the non-empty assertion of DESIGN.md §2.5.
// An unmounted share answers every listing with nothing, and a sweep on that
// answer erases the only record of what the publisher has - after which a mirror
// pair would delete 30 TB.
func TestScanRefusesAnEmptyRemoteRoot(t *testing.T) {
	h := newHarness(t)
	writeTree(h)

	first := h.scan()
	before := h.manifest(h.pair)

	entries, err := os.ReadDir(h.src)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(h.src, e.Name())); err != nil {
			t.Fatalf("remove: %v", err)
		}
	}

	res, err := h.engine.Scan(context.Background(), h.pair, false)
	if err == nil {
		t.Fatal("a scan that found no files at all reported success")
	}
	if res.Scan.State != store.ScanFailed {
		t.Errorf("the scan is in state %q, want %q", res.Scan.State, store.ScanFailed)
	}
	if res.Swept != 0 {
		t.Errorf("the scan swept %d manifest rows on an empty root, want none", res.Swept)
	}

	after := h.manifest(h.pair)
	if len(after) != len(before) {
		t.Errorf("the manifest has %d rows after the empty scan, want the %d it had", len(after), len(before))
	}
	for rel := range tree {
		if _, ok := after[rel]; !ok {
			t.Errorf("%q was swept out of the manifest by a scan that found nothing", rel)
		}
	}

	// The differ has to keep seeing the last complete answer, not the failed walk.
	last, ok, err := h.store.LastCompletedScan(context.Background(), h.pair.ID)
	if err != nil || !ok {
		t.Fatalf("last completed scan: ok=%v err=%v", ok, err)
	}
	if last.ID != first.Scan.ID {
		t.Errorf("the differ would read scan %d, want the last complete one %d", last.ID, first.Scan.ID)
	}
}

// TestScanStoresPairRelativePaths keeps the two coordinate systems apart: the
// remote reports paths under the WebDAV root, the manifest holds them under the
// pair's root, and the local side is joined from the latter.
func TestScanStoresPairRelativePaths(t *testing.T) {
	h := newHarness(t)
	writeTree(h)

	pair := h.addPair(store.Pair{Name: "filme", RemotePath: "Filme"})
	res, err := h.engine.Scan(context.Background(), pair, false)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.Scan.Files != 2 {
		t.Errorf("scan found %d files, want the 2 below Filme", res.Scan.Files)
	}

	m := h.manifest(pair)
	want := map[string]bool{"": true, "A": true, "B": true, "A/Grüße.mkv": false, "B/b.mkv": false}
	if len(m) != len(want) {
		t.Errorf("the manifest has %d rows, want %d: %v", len(m), len(want), m)
	}
	for path, isDir := range want {
		e, ok := m[path]
		if !ok {
			t.Errorf("%q is missing from the manifest", path)
			continue
		}
		if e.IsDir != isDir {
			t.Errorf("%q has isDir %v, want %v", path, e.IsDir, isDir)
		}
	}

	// The pair the harness owns must not have been touched by another pair's walk.
	if other := h.manifest(h.pair); len(other) != 0 {
		t.Errorf("the unscanned pair has %d manifest rows: %v", len(other), other)
	}
}
