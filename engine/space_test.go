package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"blackforestbytes.com/jcc-mirror/store"
)

func TestFreeSpaceReadsTheVolume(t *testing.T) {
	space, err := FreeSpace(t.TempDir())
	if errors.Is(err, ErrNoStatfs) {
		t.Skip("no statfs on this platform")
	}
	if err != nil {
		t.Fatalf("FreeSpace: %v", err)
	}
	if space.Free <= 0 || space.Total <= 0 {
		t.Errorf("FreeSpace = %+v, want positive numbers", space)
	}
}

// TestFreeSpaceAnswersForAPairThatHasNoDirectoryYet is the ordinary case on a
// first run: the local root is created by the first transfer, and the preflight
// has to answer before that.
func TestFreeSpaceAnswersForAPairThatHasNoDirectoryYet(t *testing.T) {
	space, err := FreeSpace(filepath.Join(t.TempDir(), "not", "there", "yet"))
	if errors.Is(err, ErrNoStatfs) {
		t.Skip("no statfs on this platform")
	}
	if err != nil {
		t.Fatalf("FreeSpace: %v", err)
	}
	if space.Free <= 0 {
		t.Errorf("FreeSpace = %+v, want the volume the directory will be made on", space)
	}
}

// TestSyncRefusesAPlanThatWillNotFit is S4: a transfer that would eat into the
// reserve is refused before it starts rather than discovered when the volume is
// full (DESIGN.md §2.6).
func TestSyncRefusesAPlanThatWillNotFit(t *testing.T) {
	if _, err := FreeSpace(t.TempDir()); errors.Is(err, ErrNoStatfs) {
		t.Skip("no statfs on this platform")
	}

	// A reserve nothing can satisfy, which is the same arithmetic as a plan
	// larger than the volume and needs no 30 TB to exercise.
	h := newHarness(t, func(o *Options) { o.Reserve = 1 << 62 })
	h.write("Filme/big.mkv", 4096)
	h.scan()

	if _, err := h.engine.Enqueue(context.Background(), h.pair); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	_, err := h.engine.Sync(context.Background(), h.pair)

	var noSpace *SpaceError
	if !errors.As(err, &noSpace) {
		t.Fatalf("sync error = %v, want a SpaceError", err)
	}

	// Refused, not failed: the queue is untouched, so lowering the reserve and
	// running again picks it straight back up.
	jobs := h.jobs()
	if len(jobs) != 1 || jobs[0].State != store.JobPending || jobs[0].Attempts != 0 {
		t.Errorf("after the refusal the queue holds %+v, want one untouched pending job", jobs)
	}
}

// TestPlanSaysItWillNotFit is why the plan carries the numbers: a dry run has to
// be able to say no before the run that stops does.
func TestPlanSaysItWillNotFit(t *testing.T) {
	if _, err := FreeSpace(t.TempDir()); errors.Is(err, ErrNoStatfs) {
		t.Skip("no statfs on this platform")
	}

	h := newHarness(t, func(o *Options) { o.Reserve = 1 << 62 })
	h.write("Filme/big.mkv", 4096)
	h.scan()

	p, err := h.engine.Plan(context.Background(), h.pair, 0)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if p.Shortfall <= 0 {
		t.Errorf("plan reports a shortfall of %d, want the reserve it cannot meet", p.Shortfall)
	}
	if p.Free <= 0 {
		t.Errorf("plan reports %d free", p.Free)
	}
}
