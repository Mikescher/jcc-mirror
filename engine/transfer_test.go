package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"blackforestbytes.com/jcc-mirror/store"
)

// enqueueOne queues the pair's transfers and returns the only job there is.
func enqueueOne(h *harness) store.Job {
	h.t.Helper()

	if _, err := h.engine.Enqueue(context.Background(), h.pair); err != nil {
		h.t.Fatalf("enqueue: %v", err)
	}
	jobs := h.jobs()
	if len(jobs) != 1 {
		h.t.Fatalf("the queue holds %d jobs, want 1", len(jobs))
	}
	return jobs[0]
}

func writePart(h *harness, job store.Job, body []byte) string {
	h.t.Helper()

	part := partPath(h.pair, job.Path)
	if err := os.MkdirAll(filepath.Dir(part), 0o755); err != nil {
		h.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(part, body, 0o644); err != nil {
		h.t.Fatalf("write the partial file: %v", err)
	}
	return part
}

// TestTransferMovesAMultiChunkFile runs several rounds of parallel ranged
// streams over one file, which is the shape of every real transfer: one file at
// a time, 2-4 chunks inside it (DESIGN.md §2.4).
func TestTransferMovesAMultiChunkFile(t *testing.T) {
	const (
		chunkSize = 1024
		size      = 10*chunkSize + 7 // a short last chunk, which is the off-by-one that hurts
	)

	h := newHarness(t, func(o *Options) { o.Chunks, o.ChunkSize = 3, chunkSize })
	body := h.write("Filme/big.mkv", size)
	h.scan()

	w := h.wrap()
	res := h.sync()
	if res.Files != 1 {
		t.Errorf("sync moved %d files, want 1", res.Files)
	}
	h.wantFile("Filme/big.mkv", body)
	h.wantNoStaging()

	// The publisher's timestamp exactly as the manifest holds it, not the download
	// time: the next scan diffs against that number, and a file that misses it
	// transfers again on every scan, forever (DESIGN.md §2.3).
	entry, ok, err := h.store.ManifestEntryAt(context.Background(), h.pair.ID, "Filme/big.mkv")
	if err != nil || !ok {
		t.Fatalf("manifest entry: ok=%v err=%v", ok, err)
	}
	st, err := os.Stat(filepath.Join(h.dst, filepath.FromSlash("Filme/big.mkv")))
	if err != nil {
		t.Fatalf("stat the destination: %v", err)
	}
	if !st.ModTime().Equal(entry.MTime) {
		t.Errorf("the file landed with mtime %s, want the publisher's %s", st.ModTime(), entry.MTime)
	}

	// Each byte fetched exactly once: a stream that ran to the end of the file
	// instead of to the end of its chunk would fetch the tail once per stream.
	if got := w.fetched(); got != size {
		t.Errorf("the engine fetched %d bytes, want %d", got, size)
	}
	wantChunks := (size + chunkSize - 1) / chunkSize
	if got := len(w.requests()); got != wantChunks {
		t.Errorf("the engine made %d ranged requests, want %d", got, wantChunks)
	}
	for _, r := range w.requests() {
		if r.length > chunkSize {
			t.Errorf("a ranged request asked for %d bytes, more than one chunk of %d", r.length, chunkSize)
		}
	}
}

// TestTransferResumesFromTheWatermark is what makes an interrupted 40 GB file
// cost the rest of itself rather than all of itself.
func TestTransferResumesFromTheWatermark(t *testing.T) {
	const (
		chunkSize = 1024
		size      = 8 * chunkSize
		done      = 3 * chunkSize
	)

	h := newHarness(t, func(o *Options) { o.Chunks, o.ChunkSize = 2, chunkSize })
	body := h.write("Filme/big.mkv", size)
	h.scan()

	job := enqueueOne(h)
	writePart(h, job, body[:done])
	if err := h.store.JobProgress(context.Background(), job.ID, done); err != nil {
		t.Fatalf("job progress: %v", err)
	}

	w := h.wrap()
	res, err := h.engine.Sync(context.Background(), h.pair)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Files != 1 {
		t.Errorf("sync moved %d files, want 1", res.Files)
	}
	h.wantFile("Filme/big.mkv", body)

	if got := w.fetched(); got != size-done {
		t.Errorf("the engine fetched %d bytes, want only the missing %d", got, size-done)
	}
	if got := w.lowestOffset(); got != done {
		t.Errorf("the earliest byte fetched is %d, want the watermark %d", got, done)
	}
}

// TestTransferResumeTrustsTheFileOverTheWatermark: the .part may have been
// cleaned out from under the watermark, and writing at an offset past the end of
// it would leave a hole that the size check cannot see.
func TestTransferResumeTrustsTheFileOverTheWatermark(t *testing.T) {
	const (
		chunkSize = 1024
		size      = 8 * chunkSize
		onDisk    = 2 * chunkSize
		claimed   = 6 * chunkSize
	)

	h := newHarness(t, func(o *Options) { o.Chunks, o.ChunkSize = 2, chunkSize })
	body := h.write("Filme/big.mkv", size)
	h.scan()

	job := enqueueOne(h)
	writePart(h, job, body[:onDisk])
	if err := h.store.JobProgress(context.Background(), job.ID, claimed); err != nil {
		t.Fatalf("job progress: %v", err)
	}

	w := h.wrap()
	if _, err := h.engine.Sync(context.Background(), h.pair); err != nil {
		t.Fatalf("sync: %v", err)
	}
	h.wantFile("Filme/big.mkv", body)

	if got := w.lowestOffset(); got != onDisk {
		t.Errorf("the earliest byte fetched is %d, want the %d that are actually there", got, onDisk)
	}
	if got := w.fetched(); got != size-onDisk {
		t.Errorf("the engine fetched %d bytes, want %d", got, size-onDisk)
	}
}

// TestTransferDiscardsAPartOfAnOlderVersion: a .part with a zero watermark holds
// bytes of a version that no longer exists. Appending to it would produce a file
// of exactly the right size and the wrong content.
func TestTransferDiscardsAPartOfAnOlderVersion(t *testing.T) {
	const (
		chunkSize = 1024
		size      = 4 * chunkSize
	)

	h := newHarness(t, func(o *Options) {
		o.Chunks, o.ChunkSize, o.MaxAttempts = 1, chunkSize, 1
	})
	body := h.write("Filme/big.mkv", size)
	h.scan()

	job := enqueueOne(h)
	part := writePart(h, job, bytes.Repeat([]byte{0xAA}, 6*chunkSize))

	w := h.wrap()
	var reads atomic.Int64
	w.onRange = func(string, int64, int64) error {
		if reads.Add(1) > 1 {
			return errors.New("the publisher went away")
		}
		return nil
	}

	res, err := h.engine.Sync(context.Background(), h.pair)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Failed != 1 {
		t.Errorf("sync reports %d failures, want 1", res.Failed)
	}

	// One chunk was written over an emptied file, so that is all there may be: the
	// older version's tail must be gone rather than waiting to be counted as good.
	st, err := os.Stat(part)
	if err != nil {
		t.Fatalf("stat the partial file: %v", err)
	}
	if st.Size() != chunkSize {
		t.Errorf("the partial file is %d bytes after one chunk, want %d", st.Size(), chunkSize)
	}

	w.onRange = nil
	if _, err := h.engine.Enqueue(context.Background(), h.pair); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := h.engine.Sync(context.Background(), h.pair); err != nil {
		t.Fatalf("sync: %v", err)
	}
	h.wantFile("Filme/big.mkv", body)
	h.wantNoStaging()
}

// TestSyncAdoptsAMatchingDestination is what a crash between the rename and the
// database write leaves behind: the file is already there and correct, and
// re-fetching it would be the expensive way to find that out.
func TestSyncAdoptsAMatchingDestination(t *testing.T) {
	h := newHarness(t)
	body := h.write("Filme/a.mkv", 4096)
	h.scan()

	entry, ok, err := h.store.ManifestEntryAt(context.Background(), h.pair.ID, "Filme/a.mkv")
	if err != nil || !ok {
		t.Fatalf("manifest entry: ok=%v err=%v", ok, err)
	}
	h.local("Filme/a.mkv", body, entry.MTime)

	w := h.wrap()
	res := h.sync()
	if res.InPlace != 1 || res.Files != 0 {
		t.Errorf("sync moved %d files and adopted %d in place, want 0 and 1", res.Files, res.InPlace)
	}
	if got := len(w.requests()); got != 0 {
		t.Errorf("the engine made %d ranged requests for a file already on disk", got)
	}
	h.wantNoStaging()

	f, ok, err := h.store.FileAt(context.Background(), h.pair.ID, "Filme/a.mkv")
	if err != nil || !ok {
		t.Fatalf("local truth: ok=%v err=%v", ok, err)
	}
	if f.State != store.FileAdopted {
		t.Errorf("the file is recorded as %q, want %q - it was never transferred by us", f.State, store.FileAdopted)
	}
	if f.Size != int64(len(body)) || !f.MTime.Equal(entry.MTime) {
		t.Errorf("local truth is %d bytes at %s, want %d at the publisher's %s", f.Size, f.MTime, len(body), entry.MTime)
	}
}

// TestSyncRetriesThenGivesUp: every file carries its own retry budget, and a
// file that runs out of it has to end up somewhere a human can find it.
func TestSyncRetriesThenGivesUp(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.MaxAttempts, o.RetryBackoff = 2, 20*time.Millisecond
	})
	h.write("Filme/a.mkv", 1024)
	h.scan()

	w := h.wrap()
	w.onRange = func(string, int64, int64) error { return errors.New("the publisher went away") }

	if _, err := h.engine.Enqueue(context.Background(), h.pair); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// The second attempt is due after the backoff, which the run either waits out
	// itself or leaves to the next one.
	var failed, retrying int
	for round := 1; round <= 3; round++ {
		res, err := h.engine.Sync(context.Background(), h.pair)
		if err != nil {
			t.Fatalf("sync round %d: %v", round, err)
		}
		failed += res.Failed
		retrying += res.Retrying
		if res.Pending == 0 {
			break
		}
	}
	if failed != 1 {
		t.Errorf("%d files gave up, want 1", failed)
	}
	if retrying != 1 {
		t.Errorf("%d attempts were rescheduled, want 1 - the file has to be retried before it is given up on", retrying)
	}

	job := h.jobs()[0]
	if job.State != store.JobFailed {
		t.Errorf("the job is in state %q, want %q", job.State, store.JobFailed)
	}
	if job.Attempts != 2 {
		t.Errorf("the job was attempted %d times, want the 2 it was allowed", job.Attempts)
	}
	if job.Error == "" {
		t.Error("the job carries no error to look at")
	}

	changes, err := h.store.Changes(context.Background(), store.ChangeFilter{PairID: &h.pair.ID, Ops: []string{store.ChangeFail}})
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("the history holds %d fail rows, want 1", len(changes))
	}
	if changes[0].Path != "Filme/a.mkv" || changes[0].Error == "" {
		t.Errorf("the fail row is %+v, want Filme/a.mkv with a reason", changes[0])
	}
	if _, err := os.Stat(filepath.Join(h.dst, filepath.FromSlash("Filme/a.mkv"))); !os.IsNotExist(err) {
		t.Errorf("a failed transfer left something at the destination: %v", err)
	}
}

// TestSyncRequeuesAJobLeftRunning: a crash, a self-update or a closed transfer
// window leaves rows in 'running'. Requeuing them at the start of the next run
// is what makes a transfer retriable at all (DESIGN.md §2.4).
func TestSyncRequeuesAJobLeftRunning(t *testing.T) {
	h := newHarness(t)
	body := h.write("Filme/a.mkv", 4096)
	h.scan()

	job := enqueueOne(h)
	if err := h.store.SetJobState(context.Background(), job.ID, store.JobRunning); err != nil {
		t.Fatalf("set job state: %v", err)
	}

	res, err := h.engine.Sync(context.Background(), h.pair)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Files != 1 {
		t.Errorf("sync moved %d files, want the 1 left in flight", res.Files)
	}
	h.wantFile("Filme/a.mkv", body)
	h.wantNoStaging()

	if got := h.jobs()[0]; got.State != store.JobDone {
		t.Errorf("the job is in state %q, want %q", got.State, store.JobDone)
	}
}
