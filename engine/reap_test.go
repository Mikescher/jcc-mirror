package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"blackforestbytes.com/jcc-mirror/store"
)

// mirror switches the harness pair to the mode that deletes, with the guards the
// caller wants to exercise.
func (h *harness) mirror(guard int) {
	h.t.Helper()

	h.pair.Mode = store.ModeMirror
	h.pair.DeleteGuard = guard
	if err := h.store.UpdatePair(context.Background(), &h.pair); err != nil {
		h.t.Fatalf("update pair: %v", err)
	}
}

// drop removes a file from the publisher's side, which is the only way a file
// ever becomes a candidate for deletion here.
func (h *harness) drop(rel string) {
	h.t.Helper()
	if err := os.Remove(filepath.Join(h.src, filepath.FromSlash(rel))); err != nil {
		h.t.Fatalf("remove %s: %v", rel, err)
	}
}

func (h *harness) reap() ReapResult {
	h.t.Helper()

	res, err := h.engine.Reap(context.Background(), h.pair)
	if err != nil {
		h.t.Fatalf("reap: %v", err)
	}
	return res
}

func (h *harness) exists(rel string) bool {
	h.t.Helper()
	_, err := os.Stat(filepath.Join(h.dst, filepath.FromSlash(rel)))
	return err == nil
}

// TestMirrorQuarantinesWhatThePublisherDropped is the whole of §2.5 in one run:
// the file leaves the tree, it is not unlinked, local truth stops claiming it,
// and it can be put back.
func TestMirrorQuarantinesWhatThePublisherDropped(t *testing.T) {
	h := newHarness(t)
	h.mirror(0)
	h.write("Filme/gone.mkv", 2048)
	h.write("Filme/stays.mkv", 1024)
	h.scan()
	h.sync()

	h.drop("Filme/gone.mkv")
	h.scan()

	res := h.reap()
	if res.Deleted != 1 || res.Bytes != 2048 {
		t.Fatalf("reap deleted %d files (%d bytes), want 1 (2048): %+v", res.Deleted, res.Bytes, res)
	}
	if h.exists("Filme/gone.mkv") {
		t.Error("the deleted file is still in the tree")
	}
	if !h.exists("Filme/stays.mkv") {
		t.Error("a file the publisher still has was deleted")
	}

	if _, ok, err := h.store.FileAt(context.Background(), h.pair.ID, "Filme/gone.mkv"); err != nil || ok {
		t.Errorf("local truth still holds the deleted file: ok=%v err=%v", ok, err)
	}

	days, err := Trash(h.pair, 0)
	if err != nil {
		t.Fatalf("trash: %v", err)
	}
	if len(days) != 1 || days[0].Files != 1 || days[0].Bytes != 2048 {
		t.Fatalf("the quarantine holds %+v, want one day with one 2048-byte file", days)
	}

	// Nothing was unlinked, which is the property the whole design of §2.5 rests
	// on: the answer to a deletion that should not have happened is a move back.
	back, err := RestoreTrash(h.pair, "", "Filme/gone.mkv")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if back.Size != 2048 || !h.exists("Filme/gone.mkv") {
		t.Errorf("restore returned %+v but the file is not back", back)
	}

	changes, err := h.store.Changes(context.Background(), store.ChangeFilter{PairID: &h.pair.ID, Ops: []string{store.ChangeDelete}})
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "Filme/gone.mkv" {
		t.Errorf("the history records %+v, want one delete of Filme/gone.mkv", changes)
	}
}

// TestAdditivePairNeverDeletes is the default, and the reason mirror mode is not:
// until the diff is trusted, a publisher who unplugs a disk costs nothing here.
func TestAdditivePairNeverDeletes(t *testing.T) {
	h := newHarness(t)
	h.write("Filme/gone.mkv", 512)
	h.write("Filme/stays.mkv", 512)
	h.scan()
	h.sync()

	h.drop("Filme/gone.mkv")
	h.scan()

	res := h.reap()
	if res.Deleted != 0 || !strings.Contains(res.Skipped, "additive") {
		t.Fatalf("reap of an additive pair = %+v, want nothing deleted and a reason", res)
	}
	if !h.exists("Filme/gone.mkv") {
		t.Error("an additive pair deleted a file")
	}
}

// TestTheDeletionThresholdHoldsTheWholeSet is the first guard: over the limit,
// nothing at all goes - not the first N and then a stop.
func TestTheDeletionThresholdHoldsTheWholeSet(t *testing.T) {
	h := newHarness(t)
	h.mirror(1)
	for _, rel := range []string{"a.mkv", "b.mkv", "c.mkv"} {
		h.write(rel, 128)
	}
	h.scan()
	h.sync()

	h.drop("a.mkv")
	h.drop("b.mkv")
	h.scan()

	res := h.reap()
	if res.Blocked == nil {
		t.Fatalf("reap = %+v, want a blocked deletion", res)
	}
	if res.Deleted != 0 || !h.exists("a.mkv") || !h.exists("b.mkv") {
		t.Fatalf("a blocked deletion removed %d file(s); a threshold that deletes some of the set is not a threshold", res.Deleted)
	}
	if res.Blocked.Files != 2 || res.Blocked.State != store.ApprovalPending {
		t.Errorf("the request is %+v, want 2 pending files", res.Blocked)
	}

	// The plan says the same thing without running anything, which is where an
	// operator meets this first.
	plan, err := h.engine.Plan(context.Background(), h.pair, 0)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Guard == "" || plan.Approved {
		t.Errorf("plan reports guard=%q approved=%v, want an unapproved guard", plan.Guard, plan.Approved)
	}

	// A second run does not stack a second request: one pair, one thing to answer.
	if again := h.reap(); again.Blocked == nil || again.Blocked.ID != res.Blocked.ID {
		t.Errorf("the second run raised %+v, want the same request as the first", again.Blocked)
	}

	if _, err := h.store.DecideDeletion(context.Background(), h.pair.ID, store.ApprovalApproved, "test"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	done := h.reap()
	if done.Deleted != 2 || done.Approved == nil {
		t.Fatalf("after approval reap = %+v, want both files quarantined", done)
	}
	if h.exists("a.mkv") || h.exists("b.mkv") {
		t.Error("the approved files are still in the tree")
	}

	// Spent, not standing: what vanishes next is a new decision.
	if _, ok, err := h.store.OpenApproval(context.Background(), h.pair.ID); err != nil || ok {
		t.Errorf("the approval is still open after being used: ok=%v err=%v", ok, err)
	}
}

// TestAnApprovalDoesNotSurviveANewWalk is what binds an approval to what was
// looked at. A publisher who drops another thousand files after the operator said
// yes must not have that yes apply to them.
func TestAnApprovalDoesNotSurviveANewWalk(t *testing.T) {
	h := newHarness(t)
	h.mirror(1)
	for _, rel := range []string{"a.mkv", "b.mkv", "c.mkv", "d.mkv"} {
		h.write(rel, 128)
	}
	h.scan()
	h.sync()

	h.drop("a.mkv")
	h.drop("b.mkv")
	h.scan()

	if res := h.reap(); res.Blocked == nil {
		t.Fatalf("reap = %+v, want a blocked deletion", res)
	}
	if _, err := h.store.DecideDeletion(context.Background(), h.pair.ID, store.ApprovalApproved, "test"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	h.drop("c.mkv")
	h.scan()

	res := h.reap()
	if res.Blocked == nil || res.Deleted != 0 {
		t.Fatalf("reap after a new walk = %+v, want the approval to have gone stale", res)
	}
	if res.Blocked.Files != 3 || res.Blocked.State != store.ApprovalPending {
		t.Errorf("the request is %+v, want 3 files waiting again", res.Blocked)
	}
	// a.mkv was in the approved set and c.mkv was not; neither may go on an
	// approval that no longer describes the set, so nothing at all may have moved.
	if !h.exists("a.mkv") || !h.exists("c.mkv") {
		t.Error("a stale approval deleted something")
	}
}

// TestThePercentageThresholdTripsOnItsOwn is the guard that catches the failure
// the file count does not: a small pair losing most of itself.
func TestThePercentageThresholdTripsOnItsOwn(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.DeletePercent = 10 })
	h.mirror(0) // no count limit at all: the percentage is the only guard left
	for _, rel := range []string{"a.mkv", "b.mkv", "c.mkv", "d.mkv"} {
		h.write(rel, 128)
	}
	h.scan()
	h.sync()

	h.drop("a.mkv")
	h.scan()

	res := h.reap()
	if res.Blocked == nil || !strings.Contains(res.Blocked.Reason, "10%") {
		t.Fatalf("reap = %+v, want the percentage guard to have stopped it", res)
	}
}

// TestDeletionWaitsForTheAdditionsToLand is "delete after, never during": a queue
// that still holds work is a tree that has not finished growing.
func TestDeletionWaitsForTheAdditionsToLand(t *testing.T) {
	h := newHarness(t)
	h.mirror(0)
	h.write("gone.mkv", 128)
	h.scan()
	h.sync()

	h.drop("gone.mkv")
	h.write("new.mkv", 128)
	h.scan()

	if _, err := h.engine.Enqueue(context.Background(), h.pair); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	res := h.reap()
	if res.Deleted != 0 || !strings.Contains(res.Skipped, "queued") {
		t.Fatalf("reap with a full queue = %+v, want it to wait", res)
	}
	if !h.exists("gone.mkv") {
		t.Error("the tree shrank before it had finished growing")
	}
}

// TestDeletionRefusesAnEmptyManifest is the non-empty assertion on the acting
// side: a publisher whose share is not mounted has not deleted his collection.
func TestDeletionRefusesAnEmptyManifest(t *testing.T) {
	h := newHarness(t)
	h.mirror(0)
	h.write("gone.mkv", 128)
	h.scan()
	h.sync()

	if _, err := h.store.DB().ExecContext(context.Background(),
		`DELETE FROM manifest WHERE pair_id = ?`, h.pair.ID); err != nil {
		t.Fatalf("empty the manifest: %v", err)
	}

	_, err := h.engine.Reap(context.Background(), h.pair)
	if err == nil {
		t.Fatal("reap on an empty manifest succeeded, want a refusal")
	}
	if !h.exists("gone.mkv") {
		t.Error("an empty manifest emptied the tree")
	}
}

// TestDeletionLeavesTheDirectoriesTidy: a series whose last episode is retired
// should not leave an empty directory for Jellyfin to list.
func TestDeletionLeavesTheDirectoriesTidy(t *testing.T) {
	h := newHarness(t)
	h.mirror(0)
	h.write("Serien/Show/S01/e01.mkv", 128)
	h.write("Filme/keep.mkv", 128)
	h.scan()
	h.sync()

	h.drop("Serien/Show/S01/e01.mkv")
	h.scan()
	h.reap()

	if _, err := os.Stat(filepath.Join(h.dst, "Serien")); err == nil {
		t.Error("the emptied directory tree is still there")
	}
	if !h.exists("Filme/keep.mkv") {
		t.Error("tidying up took a directory that still had something in it")
	}
}

// TestExcludedFilesAreNotDeletions: narrowing a pair stops mirroring a subtree,
// it does not empty it.
func TestExcludedFilesAreNotDeletions(t *testing.T) {
	h := newHarness(t)
	h.mirror(0)
	h.write("Filme/a.mkv", 128)
	h.write("Serien/b.mkv", 128)
	h.scan()
	h.sync()

	h.pair.Excludes = []string{"Serien"}
	if err := h.store.UpdatePair(context.Background(), &h.pair); err != nil {
		t.Fatalf("update pair: %v", err)
	}
	h.scan()

	res := h.reap()
	if res.Deleted != 0 {
		t.Fatalf("reap deleted %d file(s) after an exclude was added, want none", res.Deleted)
	}
	if !h.exists("Serien/b.mkv") {
		t.Error("excluding a subtree deleted it")
	}
}

// TestARefusalHoldsUntilTheNextWalk: a scheduled sync comes back to the same
// vanished set every hour, and an operator who said no should be asked once, not
// once an hour.
func TestARefusalHoldsUntilTheNextWalk(t *testing.T) {
	h := newHarness(t)
	h.mirror(1)
	for _, rel := range []string{"a.mkv", "b.mkv", "c.mkv"} {
		h.write(rel, 128)
	}
	h.scan()
	h.sync()

	h.drop("a.mkv")
	h.drop("b.mkv")
	h.scan()

	if res := h.reap(); res.Blocked == nil {
		t.Fatalf("reap = %+v, want a blocked deletion", res)
	}
	if _, err := h.store.DecideDeletion(context.Background(), h.pair.ID, store.ApprovalRejected, "operator"); err != nil {
		t.Fatalf("reject: %v", err)
	}

	res := h.reap()
	if res.Blocked != nil {
		t.Errorf("the run asked again after a refusal: %+v", res.Blocked)
	}
	if res.Deleted != 0 || !strings.Contains(res.Skipped, "refused") {
		t.Fatalf("reap after a refusal = %+v, want it to stand aside", res)
	}
	if !h.exists("a.mkv") {
		t.Error("a refused deletion happened anyway")
	}

	// The next walk is a new set, and a new question.
	h.scan()
	if again := h.reap(); again.Blocked == nil {
		t.Errorf("after a new walk reap = %+v, want the question asked again", again)
	}
}
