package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The conditions that notify on an edge rather than on every tick, and the sync
// run summary that notifies once per run. They are the kinds of DESIGN.md §4.1,
// and each one is also the first half of the idempotency key sent to SCN.
const (
	NotifySyncFailed    = "sync.failed"
	NotifySyncOK        = "sync.ok"
	NotifyDeleteBlocked = "delete.blocked"
	NotifySpaceLow      = "space.low"
	NotifyLockStale     = "lock.stale"
	NotifyTunnelDown    = "tunnel.down"
	NotifyDBReplaced    = "db.replaced"
)

// NotifyState is how long a persistent condition has been true, and when it was
// last announced.
type NotifyState struct {
	Kind   string    `json:"kind"`
	PairID int64     `json:"pairId"`
	Active bool      `json:"active"`
	Since  time.Time `json:"since"`
	// A pointer because a zero time.Time is not omitted by encoding/json, and a
	// condition that has never been announced would otherwise report the year 1.
	NotifiedAt *time.Time `json:"notifiedAt,omitempty"`
	Detail     string     `json:"detail,omitempty"`
}

// NotifyTransition records where a condition stands and reports whether this is
// the edge - the tick on which it started or stopped being true. Everything else
// answers false, which is what keeps a tunnel that has been down for a day from
// spending the daily quota by itself (DESIGN.md §4.1).
//
// The first sighting of a condition that is false is not an edge: a daemon that
// starts with everything working has nothing to announce.
func (s *Store) NotifyTransition(ctx context.Context, kind string, pairID int64, active bool, detail string) (edge bool, err error) {
	now := time.Now()

	err = s.tx(ctx, func(tx *sql.Tx) error {
		var prev NotifyState
		row := tx.QueryRowContext(ctx,
			`SELECT kind, pair_id, active, since, notified_at, detail
			 FROM notify_state WHERE kind = ? AND pair_id = ?`, kind, pairID)

		known := true
		prev, err = scanNotifyState(row)
		if errors.Is(err, sql.ErrNoRows) {
			known, prev, err = false, NotifyState{Kind: kind, PairID: pairID}, nil
		}
		if err != nil {
			return err
		}

		// An unknown condition reads as inactive, so a first sighting that is
		// false is not an edge and a first sighting that is true is.
		edge = active != prev.Active
		since := prev.Since
		if !known || edge {
			since = now
		}
		notified := prev.NotifiedAt
		if edge {
			notified = &now
		}

		_, err := tx.ExecContext(ctx,
			`INSERT INTO notify_state (kind, pair_id, active, since, notified_at, detail)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT (kind, pair_id) DO UPDATE
			 SET active = excluded.active, since = excluded.since,
			     notified_at = excluded.notified_at, detail = excluded.detail`,
			kind, pairID, boolInt(active), since.UnixMilli(), unixOrZero(notified), detail)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("record notification state: %w", err)
	}
	return edge, nil
}

// NotifyStates lists every condition being tracked, for the diagnostics view.
func (s *Store) NotifyStates(ctx context.Context) ([]NotifyState, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, pair_id, active, since, notified_at, detail FROM notify_state
		 ORDER BY active DESC, since DESC`)
	if err != nil {
		return nil, fmt.Errorf("read notification state: %w", err)
	}
	defer rows.Close()

	out := []NotifyState{}
	for rows.Next() {
		st, err := scanNotifyState(rows)
		if err != nil {
			return nil, fmt.Errorf("read notification state: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func scanNotifyState(row interface{ Scan(...any) error }) (NotifyState, error) {
	var (
		st                NotifyState
		active            int64
		since, notifiedAt int64
	)
	if err := row.Scan(&st.Kind, &st.PairID, &active, &since, &notifiedAt, &st.Detail); err != nil {
		return NotifyState{}, err
	}
	st.Active, st.Since = active != 0, time.UnixMilli(since)
	if notifiedAt > 0 {
		t := time.UnixMilli(notifiedAt)
		st.NotifiedAt = &t
	}
	return st, nil
}

func unixOrZero(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.UnixMilli()
}
