package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"blackforestbytes.com/jcc-mirror/store"
)

// TestPlanWithoutAManifestIsAnError is deletion safety, not tidiness: an empty
// manifest means "the walk has not run", and a differ that read it as "the
// publisher has nothing" would report the whole pair as vanished (DESIGN.md §2.5).
func TestPlanWithoutAManifestIsAnError(t *testing.T) {
	h := newHarness(t)
	h.write("Filme/a.mkv", 10)

	if _, err := h.engine.Plan(context.Background(), h.pair, 10); !errors.Is(err, errNoManifest) {
		t.Errorf("Plan before the first scan returned %v, want errNoManifest", err)
	}
	if _, err := h.engine.Enqueue(context.Background(), h.pair); !errors.Is(err, errNoManifest) {
		t.Errorf("Enqueue before the first scan returned %v, want errNoManifest", err)
	}
	if jobs := h.jobs(); len(jobs) != 0 {
		t.Errorf("%d job(s) were queued without a manifest", len(jobs))
	}
}

func TestPlanCountsAddsAndReplaces(t *testing.T) {
	h := newHarness(t)
	h.write("Filme/a.mkv", 100)
	h.write("Filme/b.mkv", 200)
	h.scan()
	h.sync()

	// A replace is a file whose size changed on the publisher; an add is one that
	// was not there at the last sync.
	a := h.write("Filme/a.mkv", 150)
	c := h.write("Filme/c.mkv", 7)
	h.scan()

	plan, err := h.engine.Plan(context.Background(), h.pair, 10)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Add != 1 || plan.AddBytes != 7 {
		t.Errorf("plan adds %d files (%d bytes), want 1 (7)", plan.Add, plan.AddBytes)
	}
	if plan.Replace != 1 || plan.ReplaceBytes != 150 {
		t.Errorf("plan replaces %d files (%d bytes), want 1 (150)", plan.Replace, plan.ReplaceBytes)
	}
	if plan.Transfers() != 2 || plan.TransferBytes() != 157 {
		t.Errorf("plan moves %d files (%d bytes), want 2 (157)", plan.Transfers(), plan.TransferBytes())
	}
	if plan.RemoteFiles != 3 || plan.LocalFiles != 2 {
		t.Errorf("plan sees %d remote and %d local files, want 3 and 2", plan.RemoteFiles, plan.LocalFiles)
	}

	byPath := map[string]PlanEntry{}
	for _, e := range plan.Entries {
		byPath[e.Path] = e
	}
	if got := byPath["Filme/a.mkv"]; got.Op != store.OpReplace || got.Size != 150 || got.SizeBefore != 100 {
		t.Errorf("a.mkv is %+v, want a replace of 100 bytes by 150", got)
	}
	if got := byPath["Filme/c.mkv"]; got.Op != store.OpAdd || got.Size != 7 || got.SizeBefore != 0 {
		t.Errorf("c.mkv is %+v, want an add of 7 bytes", got)
	}

	if _, err := h.engine.Enqueue(context.Background(), h.pair); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	queued := map[string]string{}
	for _, j := range h.jobs() {
		if j.State == store.JobPending {
			queued[j.Path] = j.Op
		}
	}
	want := map[string]string{"Filme/a.mkv": store.OpReplace, "Filme/c.mkv": store.OpAdd}
	if len(queued) != len(want) {
		t.Errorf("the queue holds %v, want %v", queued, want)
	}
	for path, op := range want {
		if queued[path] != op {
			t.Errorf("%q is queued as %q, want %q", path, queued[path], op)
		}
	}

	h.sync()
	h.wantFile("Filme/a.mkv", a)
	h.wantFile("Filme/c.mkv", c)
}

// TestPlanCountsVanishedButNeverQueuesThem: M2 is additive-only. The vanished
// list is meant to be read by a human for a while before anything acts on it,
// and deleting is M4 behind the guards of DESIGN.md §2.5.
func TestPlanCountsVanishedButNeverQueuesThem(t *testing.T) {
	h := newHarness(t)
	h.write("Filme/a.mkv", 100)
	h.write("Filme/b.mkv", 200)
	h.scan()
	h.sync()

	if err := os.Remove(filepath.Join(h.src, filepath.FromSlash("Filme/b.mkv"))); err != nil {
		t.Fatalf("remove: %v", err)
	}
	h.scan()

	plan, err := h.engine.Enqueue(context.Background(), h.pair)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if plan.Vanished != 1 || plan.VanishedBytes != 200 {
		t.Errorf("plan reports %d vanished files (%d bytes), want 1 (200)", plan.Vanished, plan.VanishedBytes)
	}
	if plan.Transfers() != 0 {
		t.Errorf("plan wants %d transfers, want none", plan.Transfers())
	}
	if plan.Queued != 0 {
		t.Errorf("enqueue wrote %d jobs, want none", plan.Queued)
	}

	for _, j := range h.jobs() {
		if j.Op == store.OpDelete {
			t.Errorf("%q was queued as a delete, which M2 must never do", j.Path)
		}
		if j.State != store.JobDone {
			t.Errorf("%q is in state %q after a sync with nothing to do", j.Path, j.State)
		}
	}
	if _, err := os.Stat(filepath.Join(h.dst, filepath.FromSlash("Filme/b.mkv"))); err != nil {
		t.Errorf("the vanished file is gone from the destination: %v", err)
	}
}

func TestPlanKeepsExcludedPathsOutOfTheTransfers(t *testing.T) {
	h := newHarness(t)
	h.write("Filme/a.mkv", 100)
	h.write("Serien/S01/e01.mkv", 200)
	h.write("Serien/S01/e02.mkv", 300)
	h.scan()

	// The globs changed after the walk, which is the case that makes Excluded worth
	// counting: a plan that is suddenly small has to say why.
	pair := h.pair
	pair.Excludes = []string{"Serien"}

	plan, err := h.engine.Enqueue(context.Background(), pair)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if plan.Add != 1 || plan.AddBytes != 100 {
		t.Errorf("plan adds %d files (%d bytes), want 1 (100)", plan.Add, plan.AddBytes)
	}
	if plan.Excluded != 2 {
		t.Errorf("plan reports %d excluded paths, want 2", plan.Excluded)
	}
	if plan.Queued != 1 {
		t.Errorf("enqueue wrote %d jobs, want 1", plan.Queued)
	}
	for _, j := range h.jobs() {
		if j.Path != "Filme/a.mkv" {
			t.Errorf("%q was queued although the pair excludes it", j.Path)
		}
	}
}

// TestExcludingASubtreeIsNotADeletion: stopping the mirroring of a directory
// must not read as the publisher having emptied it - in M4 that would put 30 TB
// in the trash directory.
func TestExcludingASubtreeIsNotADeletion(t *testing.T) {
	h := newHarness(t)
	h.write("Filme/a.mkv", 100)
	h.write("Serien/S01/e01.mkv", 200)
	h.write("Serien/S01/e02.mkv", 300)
	h.scan()
	h.sync()

	pair := h.pair
	pair.Excludes = []string{"Serien"}
	if _, err := h.engine.Scan(context.Background(), pair, false); err != nil {
		t.Fatalf("scan: %v", err)
	}

	plan, err := h.engine.Plan(context.Background(), pair, 10)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Vanished != 0 {
		t.Errorf("plan reports %d vanished files after an exclude, want none: %+v", plan.Vanished, plan.Entries)
	}
	if plan.Excluded != 2 {
		t.Errorf("plan reports %d excluded paths, want 2", plan.Excluded)
	}
	if plan.Transfers() != 0 {
		t.Errorf("plan wants %d transfers, want none", plan.Transfers())
	}
}

// TestEnqueueIsIdempotent: a scan runs every night, and each one plans the same
// outstanding transfers. Queuing them twice would mean transferring them twice.
func TestEnqueueIsIdempotent(t *testing.T) {
	h := newHarness(t)
	for _, rel := range []string{"Filme/a.mkv", "Filme/b.mkv", "Serien/e01.mkv"} {
		h.write(rel, 64)
	}
	h.scan()

	for round := 1; round <= 2; round++ {
		plan, err := h.engine.Enqueue(context.Background(), h.pair)
		if err != nil {
			t.Fatalf("enqueue round %d: %v", round, err)
		}
		if plan.Queued != 3 {
			t.Errorf("enqueue round %d wrote %d jobs, want 3", round, plan.Queued)
		}
	}

	seen := map[string]int{}
	for _, j := range h.jobs() {
		if j.State != store.JobPending {
			t.Errorf("%q is in state %q before any sync", j.Path, j.State)
		}
		seen[j.Path]++
	}
	if len(seen) != 3 {
		t.Errorf("the queue holds %d distinct files, want 3", len(seen))
	}
	for path, n := range seen {
		if n != 1 {
			t.Errorf("%q is queued %d times, want once", path, n)
		}
	}
}

func TestPlanSampleCapsTheEntries(t *testing.T) {
	h := newHarness(t)
	for _, rel := range []string{"a.mkv", "b.mkv", "c.mkv", "d.mkv", "e.mkv"} {
		h.write(rel, 8)
	}
	h.scan()

	tests := []struct {
		name      string
		sample    int
		entries   int
		truncated bool
	}{
		{"no sample at all", 0, 0, false},
		{"a sample smaller than the plan", 2, 2, true},
		{"a sample the size of the plan", 5, 5, false},
		{"a sample larger than the plan", 50, 5, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := h.engine.Plan(context.Background(), h.pair, tt.sample)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if len(plan.Entries) != tt.entries {
				t.Errorf("plan carries %d entries, want %d", len(plan.Entries), tt.entries)
			}
			if plan.Truncated != tt.truncated {
				t.Errorf("plan reports truncated %v, want %v", plan.Truncated, tt.truncated)
			}
			// The counts are complete whatever the sample is: they are what a free
			// space check and a delete guard are decided on.
			if plan.Add != 5 {
				t.Errorf("plan adds %d files, want 5", plan.Add)
			}
		})
	}
}
