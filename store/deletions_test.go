package store

import (
	"context"
	"errors"
	"testing"
)

// pairForDeletion is a pair to hang approvals off. The foreign key is real, so
// there has to be one.
func pairForDeletion(t *testing.T, s *Store) Pair {
	t.Helper()

	p := Pair{Name: "media", LocalPath: "/mnt/media", Mode: ModeGuarded, Enabled: true}
	if err := s.CreatePair(context.Background(), &p); err != nil {
		t.Fatalf("create pair: %v", err)
	}
	return p
}

func TestApprovalCoversOnlyWhatWasLookedAt(t *testing.T) {
	approved := DeleteApproval{State: ApprovalApproved, ScanID: 7, Files: 100}

	for _, tc := range []struct {
		name   string
		scanID int64
		files  int64
		want   bool
	}{
		{"the set it was granted for", 7, 100, true},
		{"a set that shrank since", 7, 40, true},
		{"a set that grew since", 7, 101, false},
		{"a later walk", 8, 100, false},
	} {
		if got := approved.Covers(tc.scanID, tc.files); got != tc.want {
			t.Errorf("%s: Covers(%d, %d) = %v, want %v", tc.name, tc.scanID, tc.files, got, tc.want)
		}
	}

	pending := DeleteApproval{State: ApprovalPending, ScanID: 7, Files: 100}
	if pending.Covers(7, 100) {
		t.Error("a request nobody has answered covers a deletion")
	}
}

func TestOneOpenRequestPerPair(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	pair := pairForDeletion(t, s)

	first, err := s.RequestDeleteApproval(ctx, DeleteApproval{PairID: pair.ID, ScanID: 1, Files: 10, Bytes: 1000, Reason: "over the guard"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	// A second walk with different numbers updates the request rather than adding
	// one: an operator answers one question per pair, and it is the current one.
	second, err := s.RequestDeleteApproval(ctx, DeleteApproval{PairID: pair.ID, ScanID: 2, Files: 30, Bytes: 3000, Reason: "over the guard"})
	if err != nil {
		t.Fatalf("request again: %v", err)
	}
	if second.ID != first.ID || second.Files != 30 || second.ScanID != 2 {
		t.Errorf("the second request is %+v, want the first row updated to 30 files of walk 2", second)
	}

	all, err := s.Approvals(ctx, pair.ID, 10)
	if err != nil {
		t.Fatalf("approvals: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("the pair has %d requests, want one", len(all))
	}
}

// TestANewWalkVoidsAnApproval is the safety property the whole table exists for:
// a yes applies to what was looked at, and nothing else.
func TestANewWalkVoidsAnApproval(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	pair := pairForDeletion(t, s)

	if _, err := s.RequestDeleteApproval(ctx, DeleteApproval{PairID: pair.ID, ScanID: 1, Files: 10, Reason: "over the guard"}); err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := s.DecideDeletion(ctx, pair.ID, ApprovalApproved, "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// The same set from the same walk is still covered, so a run that was
	// interrupted between the approval and the deletion picks it back up.
	same, err := s.RequestDeleteApproval(ctx, DeleteApproval{PairID: pair.ID, ScanID: 1, Files: 10, Reason: "over the guard"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if same.State != ApprovalApproved {
		t.Errorf("the standing approval became %q, want it left alone", same.State)
	}

	// A later walk is a different set, and takes the answer back.
	next, err := s.RequestDeleteApproval(ctx, DeleteApproval{PairID: pair.ID, ScanID: 2, Files: 10, Reason: "over the guard"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if next.State != ApprovalPending || next.DecidedAt != nil {
		t.Errorf("after a new walk the request is %+v, want it waiting again", next)
	}
}

func TestAnApprovalIsSpentOnce(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	pair := pairForDeletion(t, s)

	req, err := s.RequestDeleteApproval(ctx, DeleteApproval{PairID: pair.ID, ScanID: 1, Files: 10, Reason: "over the guard"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := s.DecideDeletion(ctx, pair.ID, ApprovalApproved, "operator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := s.UseApproval(ctx, req.ID); err != nil {
		t.Fatalf("use: %v", err)
	}

	if _, ok, err := s.OpenApproval(ctx, pair.ID); err != nil || ok {
		t.Errorf("a spent approval is still open: ok=%v err=%v", ok, err)
	}
	if _, err := s.DecideDeletion(ctx, pair.ID, ApprovalApproved, "operator"); !errors.Is(err, ErrNoApproval) {
		t.Errorf("deciding a spent request returned %v, want ErrNoApproval", err)
	}
}

func TestRejectionLeavesNothingOpen(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	pair := pairForDeletion(t, s)

	if _, err := s.RequestDeleteApproval(ctx, DeleteApproval{PairID: pair.ID, ScanID: 1, Files: 10, Reason: "over the guard"}); err != nil {
		t.Fatalf("request: %v", err)
	}
	rejected, err := s.DecideDeletion(ctx, pair.ID, ApprovalRejected, "operator")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if rejected.State != ApprovalRejected || rejected.DecidedBy != "operator" {
		t.Errorf("the rejection is %+v, want it recorded against the operator", rejected)
	}
	if _, ok, err := s.OpenApproval(ctx, pair.ID); err != nil || ok {
		t.Errorf("a rejected request is still open: ok=%v err=%v", ok, err)
	}
}
