package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Event levels.
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Event kinds. These are the rows the Events view lists and the SSE stream
// replays, so they are a closed vocabulary rather than free text (DESIGN.md §4).
const (
	KindStartup       = "startup"
	KindShutdown      = "shutdown"
	KindConfigChanged = "config.changed"
	KindTunnelUp      = "tunnel.up"
	KindTunnelDown    = "tunnel.down"
	KindRemoteProbe   = "remote.probe"
	KindPairChanged   = "pair.changed"
	KindScanStarted   = "scan.started"
	KindScanFinished  = "scan.finished"
	KindSyncStarted   = "sync.started"
	KindSyncFinished  = "sync.finished"
	KindTransferFail  = "transfer.failed"
	KindAdopted       = "adopt.finished"
	KindWindow        = "schedule.window"
	KindSpaceLow      = "space.low"
	KindError         = "error"
)

// Event is one row of the event log. It is also the shape the dashboard receives
// over SSE, so the live view and the history view share a schema.
type Event struct {
	ID      int64          `json:"id"`
	TS      time.Time      `json:"ts"`
	Level   string         `json:"level"`
	Kind    string         `json:"kind"`
	PairID  *int64         `json:"pairId,omitempty"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

// AppendEvent writes one event and fills in its ID and timestamp.
func (s *Store) AppendEvent(ctx context.Context, e *Event) error {
	if e.Level == "" {
		e.Level = LevelInfo
	}
	if e.TS.IsZero() {
		e.TS = time.Now()
	}

	var data any
	if len(e.Data) > 0 {
		raw, err := json.Marshal(e.Data)
		if err != nil {
			return fmt.Errorf("encode event data: %w", err)
		}
		data = string(raw)
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO events (ts, level, kind, pair_id, message, data) VALUES (?, ?, ?, ?, ?, ?)`,
		e.TS.UnixMilli(), e.Level, e.Kind, e.PairID, e.Message, data)
	if err != nil {
		return fmt.Errorf("append event %q: %w", e.Kind, err)
	}
	e.ID, err = res.LastInsertId()
	if err != nil {
		return fmt.Errorf("append event %q: %w", e.Kind, err)
	}
	return nil
}

// EventFilter selects a slice of the log. The zero value means "the most recent
// events, whatever they are".
type EventFilter struct {
	Kinds  []string
	Levels []string
	PairID *int64
	Since  time.Time
	Limit  int
}

// Events returns matching events, newest first.
func (s *Store) Events(ctx context.Context, f EventFilter) ([]Event, error) {
	var (
		where []string
		args  []any
	)
	if len(f.Kinds) > 0 {
		where = append(where, "kind IN ("+placeholders(len(f.Kinds))+")")
		for _, k := range f.Kinds {
			args = append(args, k)
		}
	}
	if len(f.Levels) > 0 {
		where = append(where, "level IN ("+placeholders(len(f.Levels))+")")
		for _, l := range f.Levels {
			args = append(args, l)
		}
	}
	if f.PairID != nil {
		where = append(where, "pair_id = ?")
		args = append(args, *f.PairID)
	}
	if !f.Since.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, f.Since.UnixMilli())
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}

	q := `SELECT id, ts, level, kind, pair_id, message, data FROM events`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()

	out := make([]Event, 0, limit)
	for rows.Next() {
		var (
			e      Event
			ms     int64
			pairID sql.NullInt64
			data   sql.NullString
		)
		if err := rows.Scan(&e.ID, &ms, &e.Level, &e.Kind, &pairID, &e.Message, &data); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.TS = time.UnixMilli(ms)
		if pairID.Valid {
			id := pairID.Int64
			e.PairID = &id
		}
		if data.Valid && data.String != "" {
			if err := json.Unmarshal([]byte(data.String), &e.Data); err != nil {
				return nil, fmt.Errorf("decode event %d data: %w", e.ID, err)
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneEvents drops events older than the retention window and returns how many
// went. Retention defaults to a year (DESIGN.md §6).
func (s *Store) PruneEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM events WHERE ts < ?`, olderThan.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("prune events: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune events: %w", err)
	}
	return n, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
