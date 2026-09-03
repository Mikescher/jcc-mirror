package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// The three resolutions a sample can be held at. The daemon only ever writes
// minutes; the coarser two are what a rollup leaves behind, so the series stays
// readable for a year without keeping half a million rows (DESIGN.md §4).
const (
	SpanMinute = "minute"
	SpanHour   = "hour"
	SpanDay    = "day"
)

// BWSample is one bucket of the bandwidth series. In is what came off the
// publisher's share and Out is what was sent to ask for it, so Out is small by
// construction: this is a mirror, not a conversation.
type BWSample struct {
	TS  time.Time `json:"ts"`
	In  int64     `json:"in"`
	Out int64     `json:"out"`
}

// Truncate rounds t down to the start of the span's bucket. Days are cut in UTC
// like everything else stored here; the weekday and hour a heatmap draws are the
// reader's problem, because they depend on the configured timezone.
func Truncate(span string, t time.Time) (time.Time, error) {
	switch span {
	case SpanMinute:
		return t.UTC().Truncate(time.Minute), nil
	case SpanHour:
		return t.UTC().Truncate(time.Hour), nil
	case SpanDay:
		u := t.UTC()
		return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC), nil
	default:
		return time.Time{}, unknownSpan(span)
	}
}

func validSpan(span string) error {
	switch span {
	case SpanMinute, SpanHour, SpanDay:
		return nil
	default:
		return unknownSpan(span)
	}
}

func unknownSpan(span string) error {
	return fmt.Errorf("unknown bandwidth span %q", span)
}

// AddBandwidth adds a delta to the minute t falls in. It adds rather than sets,
// so a sampler that runs twice in the same minute - a restart, a clock that
// stepped - accumulates instead of losing the earlier half.
func (s *Store) AddBandwidth(ctx context.Context, t time.Time, in, out int64) error {
	if in == 0 && out == 0 {
		return nil
	}
	minute, err := Truncate(SpanMinute, t)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO bw_samples (span, ts, bytes_in, bytes_out) VALUES (?, ?, ?, ?)
		 ON CONFLICT (span, ts) DO UPDATE
		 SET bytes_in = bytes_in + excluded.bytes_in, bytes_out = bytes_out + excluded.bytes_out`,
		SpanMinute, minute.UnixMilli(), in, out)
	if err != nil {
		return fmt.Errorf("record bandwidth: %w", err)
	}
	return nil
}

// Bandwidth reads one resolution of the series, oldest first, over the half-open
// interval [since, until). A zero bound is unbounded on that side.
func (s *Store) Bandwidth(ctx context.Context, span string, since, until time.Time) ([]BWSample, error) {
	if err := validSpan(span); err != nil {
		return nil, err
	}

	query := `SELECT ts, bytes_in, bytes_out FROM bw_samples WHERE span = ?`
	args := []any{span}
	if !since.IsZero() {
		query += ` AND ts >= ?`
		args = append(args, since.UnixMilli())
	}
	if !until.IsZero() {
		query += ` AND ts < ?`
		args = append(args, until.UnixMilli())
	}
	query += ` ORDER BY ts`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read bandwidth: %w", err)
	}
	defer rows.Close()

	out := []BWSample{}
	for rows.Next() {
		var ms int64
		var sample BWSample
		if err := rows.Scan(&ms, &sample.In, &sample.Out); err != nil {
			return nil, fmt.Errorf("read bandwidth: %w", err)
		}
		sample.TS = time.UnixMilli(ms)
		out = append(out, sample)
	}
	return out, rows.Err()
}

// RollupBandwidth folds every from-row older than before into to-rows and drops
// the originals. It is idempotent: a rollup that already happened finds nothing
// left to fold.
func (s *Store) RollupBandwidth(ctx context.Context, from, to string, before time.Time) (int64, error) {
	if err := validSpan(from); err != nil {
		return 0, err
	}
	if err := validSpan(to); err != nil {
		return 0, err
	}
	if from == to {
		// It would fold a span into itself and then delete what it had just
		// written, which is a data-losing no-op rather than an obvious mistake.
		return 0, fmt.Errorf("cannot roll the %s series into itself", from)
	}

	var folded int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT ts, bytes_in, bytes_out FROM bw_samples WHERE span = ? AND ts < ?`,
			from, before.UnixMilli())
		if err != nil {
			return err
		}
		defer rows.Close()

		// Bucketed in Go rather than in SQL: the truncation has to agree exactly
		// with what Truncate does, and sqlite's date functions would be a second
		// place for that to be decided.
		buckets := map[int64]BWSample{}
		for rows.Next() {
			var ms int64
			var sample BWSample
			if err := rows.Scan(&ms, &sample.In, &sample.Out); err != nil {
				return err
			}
			key, err := Truncate(to, time.UnixMilli(ms))
			if err != nil {
				return err
			}
			agg := buckets[key.UnixMilli()]
			agg.In += sample.In
			agg.Out += sample.Out
			buckets[key.UnixMilli()] = agg
			folded++
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if folded == 0 {
			return nil
		}

		for ms, agg := range buckets {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO bw_samples (span, ts, bytes_in, bytes_out) VALUES (?, ?, ?, ?)
				 ON CONFLICT (span, ts) DO UPDATE
				 SET bytes_in = bytes_in + excluded.bytes_in, bytes_out = bytes_out + excluded.bytes_out`,
				to, ms, agg.In, agg.Out); err != nil {
				return err
			}
		}

		_, err = tx.ExecContext(ctx, `DELETE FROM bw_samples WHERE span = ? AND ts < ?`,
			from, before.UnixMilli())
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("roll up bandwidth: %w", err)
	}
	return folded, nil
}

// PruneBandwidth drops rows of one span older than the cutoff. Only the daily
// series is pruned in practice: the other two are emptied by the rollups that
// fold them, and a day row is 365 a year.
func (s *Store) PruneBandwidth(ctx context.Context, span string, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM bw_samples WHERE span = ? AND ts < ?`,
		span, olderThan.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("prune bandwidth: %w", err)
	}
	return res.RowsAffected()
}
