package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Approval states. A request is raised pending, decided once, and used at most
// once after that.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalUsed     = "used"
	ApprovalRejected = "rejected"
)

// ErrNoApproval is returned when a decision is asked for and there is nothing
// open to decide.
var ErrNoApproval = errors.New("no deletion is waiting for approval")

// DeleteApproval is a deletion that tripped a guard and is waiting for a person
// (DESIGN.md §2.5).
type DeleteApproval struct {
	ID        int64      `json:"id"`
	PairID    int64      `json:"pairId"`
	ScanID    int64      `json:"scanId"`
	Files     int64      `json:"files"`
	Bytes     int64      `json:"bytes"`
	Reason    string     `json:"reason"`
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"createdAt"`
	DecidedAt *time.Time `json:"decidedAt,omitempty"`
	DecidedBy string     `json:"decidedBy,omitempty"`
	UsedAt    *time.Time `json:"usedAt,omitempty"`
}

// Covers reports whether this approval authorises deleting files rows computed
// from the given walk. Both bounds matter: a later walk is a different set, and a
// set that grew since it was looked at is one nobody approved.
func (a DeleteApproval) Covers(scanID, files int64) bool {
	return a.State == ApprovalApproved && a.decides(scanID, files)
}

// Refuses is the same question for a no. A rejection holds for the walk it was
// made against, so a scheduled sync every hour asks once rather than every hour;
// the next walk is a new set and asks again.
func (a DeleteApproval) Refuses(scanID, files int64) bool {
	return a.State == ApprovalRejected && a.decides(scanID, files)
}

// Pending reports whether the request is still waiting to be decided.
func (a DeleteApproval) Pending() bool { return a.State == ApprovalPending }

func (a DeleteApproval) decides(scanID, files int64) bool {
	return a.ScanID == scanID && files <= a.Files
}

const approvalColumns = `id, pair_id, scan_id, files, bytes, reason, state,
                         created_at, decided_at, decided_by, used_at`

// RequestDeleteApproval raises - or updates - the pair's open request and returns
// it. An approval that no longer covers the numbers is taken back to pending
// rather than left standing: the whole point of binding it to a walk and a count
// is that nothing runs on a set nobody saw.
func (s *Store) RequestDeleteApproval(ctx context.Context, req DeleteApproval) (DeleteApproval, error) {
	now := time.Now()

	var out DeleteApproval
	err := s.tx(ctx, func(tx *sql.Tx) error {
		open, ok, err := openApprovalTx(ctx, tx, req.PairID)
		if err != nil {
			return err
		}
		if ok && open.Covers(req.ScanID, req.Files) {
			out = open
			return nil
		}

		if ok {
			row := tx.QueryRowContext(ctx,
				`UPDATE delete_approvals
				 SET scan_id = ?, files = ?, bytes = ?, reason = ?, state = ?,
				     created_at = ?, decided_at = NULL, decided_by = NULL
				 WHERE id = ?
				 RETURNING `+approvalColumns,
				req.ScanID, req.Files, req.Bytes, req.Reason, ApprovalPending, now.UnixMilli(), open.ID)
			out, err = scanApproval(row)
			return err
		}

		row := tx.QueryRowContext(ctx,
			`INSERT INTO delete_approvals (pair_id, scan_id, files, bytes, reason, state, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 RETURNING `+approvalColumns,
			req.PairID, req.ScanID, req.Files, req.Bytes, req.Reason, ApprovalPending, now.UnixMilli())
		out, err = scanApproval(row)
		return err
	})
	if err != nil {
		return DeleteApproval{}, err
	}
	return out, nil
}

// OpenApproval returns the pair's undecided or approved request, if it has one.
func (s *Store) OpenApproval(ctx context.Context, pairID int64) (DeleteApproval, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+approvalColumns+` FROM delete_approvals
		 WHERE pair_id = ? AND state IN (?, ?)`, pairID, ApprovalPending, ApprovalApproved)

	a, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return DeleteApproval{}, false, nil
	}
	return a, err == nil, err
}

// LatestApproval returns the pair's most recent request whatever became of it,
// which is what a run has to read: an approval it may spend, and a rejection it
// must not ask about again.
func (s *Store) LatestApproval(ctx context.Context, pairID int64) (DeleteApproval, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+approvalColumns+` FROM delete_approvals WHERE pair_id = ? ORDER BY id DESC LIMIT 1`, pairID)

	a, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return DeleteApproval{}, false, nil
	}
	return a, err == nil, err
}

// DecideDeletion approves or rejects the pair's open request. state is
// ApprovalApproved or ApprovalRejected; actor is whoever pressed the button, for
// the record it leaves.
func (s *Store) DecideDeletion(ctx context.Context, pairID int64, state, actor string) (DeleteApproval, error) {
	if state != ApprovalApproved && state != ApprovalRejected {
		return DeleteApproval{}, fmt.Errorf("cannot decide a deletion as %q: want %q or %q", state, ApprovalApproved, ApprovalRejected)
	}

	row := s.db.QueryRowContext(ctx,
		`UPDATE delete_approvals SET state = ?, decided_at = ?, decided_by = ?
		 WHERE pair_id = ? AND state = ?
		 RETURNING `+approvalColumns,
		state, time.Now().UnixMilli(), actor, pairID, ApprovalPending)

	a, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return DeleteApproval{}, ErrNoApproval
	}
	return a, err
}

// UseApproval spends one. An approval authorises a single run: the set it was
// granted for is gone once that run has moved it, and what vanishes next is a new
// decision.
func (s *Store) UseApproval(ctx context.Context, id int64) error {
	now := time.Now().UnixMilli()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE delete_approvals SET state = ?, used_at = ? WHERE id = ? AND state = ?`,
		ApprovalUsed, now, id, ApprovalApproved); err != nil {
		return fmt.Errorf("spend approval %d: %w", id, err)
	}
	return nil
}

// Approvals returns a pair's requests, newest first - which is the order the
// dashboard and the CLI both read them in.
func (s *Store) Approvals(ctx context.Context, pairID int64, limit int) ([]DeleteApproval, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+approvalColumns+` FROM delete_approvals WHERE pair_id = ? ORDER BY id DESC LIMIT ?`,
		pairID, limit)
	if err != nil {
		return nil, fmt.Errorf("read deletion approvals: %w", err)
	}
	defer rows.Close()

	var out []DeleteApproval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func openApprovalTx(ctx context.Context, tx *sql.Tx, pairID int64) (DeleteApproval, bool, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+approvalColumns+` FROM delete_approvals
		 WHERE pair_id = ? AND state IN (?, ?)`, pairID, ApprovalPending, ApprovalApproved)

	a, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return DeleteApproval{}, false, nil
	}
	return a, err == nil, err
}

func scanApproval(row rowScanner) (DeleteApproval, error) {
	var (
		a             DeleteApproval
		created       int64
		decided, used sql.NullInt64
		by            sql.NullString
	)
	if err := row.Scan(&a.ID, &a.PairID, &a.ScanID, &a.Files, &a.Bytes, &a.Reason, &a.State,
		&created, &decided, &by, &used); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeleteApproval{}, err
		}
		return DeleteApproval{}, fmt.Errorf("scan deletion approval: %w", err)
	}
	a.CreatedAt, a.DecidedBy = time.UnixMilli(created), by.String
	if decided.Valid {
		t := time.UnixMilli(decided.Int64)
		a.DecidedAt = &t
	}
	if used.Valid {
		t := time.UnixMilli(used.Int64)
		a.UsedAt = &t
	}
	return a, nil
}
