package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"blackforestbytes.com/jcc-mirror/store"
)

// wrongMTime is what a USB copy that did not preserve timestamps leaves behind.
// Adoption matches on size alone precisely so this does not matter; a differ that
// looked at it would call all 30 TB changed (DESIGN.md §2.4).
var wrongMTime = time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

// TestAdoptTheUSBBootstrap is the whole point of adoption: the collection
// arrives on a disk, jcc-mirror measures it against the manifest, and the first
// sync then has nothing to do.
func TestAdoptTheUSBBootstrap(t *testing.T) {
	h := newHarness(t)
	want := writeTree(h)
	h.scan()

	var total int64
	for rel, body := range want {
		h.local(rel, body, wrongMTime)
		total += int64(len(body))
	}

	res, err := h.engine.Adopt(context.Background(), h.pair)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res.Matched != len(want) || res.MatchedBytes != total {
		t.Errorf("adopt matched %d files (%d bytes), want %d (%d)", res.Matched, res.MatchedBytes, len(want), total)
	}
	if res.SizeMismatch != 0 || res.Extra != 0 || res.Missing != 0 || res.AlreadyKnown != 0 {
		t.Errorf("adopt reports %d mismatched, %d extra, %d missing, %d already known, want none of each",
			res.SizeMismatch, res.Extra, res.Missing, res.AlreadyKnown)
	}
	if res.RemoteFiles != int64(len(want)) {
		t.Errorf("adopt saw %d remote files, want %d", res.RemoteFiles, len(want))
	}

	plan, err := h.engine.Plan(context.Background(), h.pair, 10)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Transfers() != 0 {
		t.Errorf("the first sync after the bootstrap wants %d transfers, want none: %+v", plan.Transfers(), plan.Entries)
	}
	if plan.Vanished != 0 {
		t.Errorf("the plan reports %d vanished files, want none", plan.Vanished)
	}

	// The publisher's mtime is what got recorded, not the one on the disk here -
	// otherwise the next diff disagrees with the manifest all over again.
	entry, _, err := h.store.ManifestEntryAt(context.Background(), h.pair.ID, "top.mkv")
	if err != nil {
		t.Fatalf("manifest entry: %v", err)
	}
	f, ok, err := h.store.FileAt(context.Background(), h.pair.ID, "top.mkv")
	if err != nil || !ok {
		t.Fatalf("local truth: ok=%v err=%v", ok, err)
	}
	if !f.MTime.Equal(entry.MTime) {
		t.Errorf("local truth records %s, want the publisher's %s", f.MTime, entry.MTime)
	}
	if f.State != store.FileAdopted {
		t.Errorf("the file is recorded as %q, want %q - nothing was transferred", f.State, store.FileAdopted)
	}
}

// TestAdoptReportsASizeMismatch: a truncated copy is the failure a USB
// bootstrap actually produces, and the numbers have to be checkable by eye
// before the first sync runs against them.
func TestAdoptReportsASizeMismatch(t *testing.T) {
	h := newHarness(t)
	a := h.write("Filme/a.mkv", 1000)
	b := h.write("Filme/b.mkv", 500)
	h.scan()

	// The copy of a.mkv was cut short; b.mkv came over whole.
	h.local("Filme/a.mkv", a[:900], wrongMTime)
	h.local("Filme/b.mkv", b, wrongMTime)

	res, err := h.engine.Adopt(context.Background(), h.pair)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res.SizeMismatch != 1 {
		t.Errorf("adopt reports %d size mismatches, want 1", res.SizeMismatch)
	}
	if res.Matched != 1 {
		t.Errorf("adopt matched %d files, want the 1 that agrees", res.Matched)
	}

	plan, err := h.engine.Plan(context.Background(), h.pair, 10)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Transfers() != 1 {
		t.Errorf("the plan wants %d transfers, want the 1 short file: %+v", plan.Transfers(), plan.Entries)
	}

	if res := h.sync(); res.Files != 1 {
		t.Errorf("sync moved %d files, want 1", res.Files)
	}
	h.wantFile("Filme/a.mkv", a)
}

func TestAdoptLeavesExtraLocalFilesAlone(t *testing.T) {
	h := newHarness(t)
	body := h.write("Filme/a.mkv", 100)
	h.scan()

	h.local("Filme/a.mkv", body, wrongMTime)
	extra := filepath.Join(h.dst, filepath.FromSlash("Filme/mine.mkv"))
	h.local("Filme/mine.mkv", make([]byte, 42), wrongMTime)

	res, err := h.engine.Adopt(context.Background(), h.pair)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res.Extra != 1 || res.ExtraBytes != 42 {
		t.Errorf("adopt reports %d extra files (%d bytes), want 1 (42)", res.Extra, res.ExtraBytes)
	}
	if res.Matched != 1 {
		t.Errorf("adopt matched %d files, want 1", res.Matched)
	}
	if _, err := os.Stat(extra); err != nil {
		t.Errorf("the extra file was touched: %v", err)
	}
	if _, ok, err := h.store.FileAt(context.Background(), h.pair.ID, "Filme/mine.mkv"); err != nil || ok {
		t.Errorf("the extra file became local truth: ok=%v err=%v", ok, err)
	}
}

func TestAdoptWithoutAManifestIsAnError(t *testing.T) {
	h := newHarness(t)
	body := h.write("Filme/a.mkv", 100)
	h.local("Filme/a.mkv", body, wrongMTime)

	if _, err := h.engine.Adopt(context.Background(), h.pair); !errors.Is(err, errNoManifest) {
		t.Errorf("Adopt before the first scan returned %v, want errNoManifest", err)
	}
}

// TestAdoptSkipsTheStagingDirectory: .jccmirror holds half-downloaded files, not
// content, and a .part counted as local truth would be a hole in the mirror.
func TestAdoptSkipsTheStagingDirectory(t *testing.T) {
	h := newHarness(t)
	body := h.write("Filme/a.mkv", 100)
	h.scan()
	h.local("Filme/a.mkv", body, wrongMTime)

	part := filepath.Join(h.dst, PartDir, "deadbeef.part")
	if err := os.MkdirAll(filepath.Dir(part), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(part, []byte("half a file"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := h.engine.Adopt(context.Background(), h.pair)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res.Extra != 0 {
		t.Errorf("adopt counted %d extra files, want none - the staging directory is not content", res.Extra)
	}
	if _, err := os.Stat(part); err != nil {
		t.Errorf("the partial file was touched: %v", err)
	}
}

// TestAdoptDoesNotRelabelASyncedFile: an adopted row was only ever measured, a
// synced one actually passed through this process. A future scrub needs to be
// able to tell them apart.
func TestAdoptDoesNotRelabelASyncedFile(t *testing.T) {
	h := newHarness(t)
	body := h.write("Filme/a.mkv", 100)
	h.scan()
	h.sync()
	h.wantFile("Filme/a.mkv", body)

	res, err := h.engine.Adopt(context.Background(), h.pair)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res.AlreadyKnown != 1 {
		t.Errorf("adopt reports %d files already known, want 1", res.AlreadyKnown)
	}
	if res.Matched != 1 {
		t.Errorf("adopt matched %d files, want 1", res.Matched)
	}

	f, ok, err := h.store.FileAt(context.Background(), h.pair.ID, "Filme/a.mkv")
	if err != nil || !ok {
		t.Fatalf("local truth: ok=%v err=%v", ok, err)
	}
	if f.State != store.FileSynced {
		t.Errorf("the file is recorded as %q after an adoption, want %q", f.State, store.FileSynced)
	}
}
