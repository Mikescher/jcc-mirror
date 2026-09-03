package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Job operations. 'delete' exists in the schema from the start but is never
// queued before M4.
const (
	OpAdd     = "add"
	OpReplace = "replace"
	OpDelete  = "delete"
)

// Job states, in the order a transfer moves through them (DESIGN.md §2.4).
const (
	JobPending   = "pending"
	JobRunning   = "running"
	JobVerifying = "verifying"
	JobDone      = "done"
	JobFailed    = "failed"
)

// Job is one file's transfer, with its own retry budget.
type Job struct {
	ID            int64     `json:"id"`
	PairID        int64     `json:"pairId"`
	Path          string    `json:"path"`
	Op            string    `json:"op"`
	BytesTotal    int64     `json:"bytesTotal"`
	BytesDone     int64     `json:"bytesDone"` // the resume watermark: the .part prefix known to be good
	MTime         time.Time `json:"mtime"`     // the publisher's, stamped on the file before the rename
	State         string    `json:"state"`
	Attempts      int       `json:"attempts"`
	NextAttemptAt time.Time `json:"nextAttemptAt,omitempty"`
	Error         string    `json:"error,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// EnqueueJob adds a transfer, or refreshes the one already queued for the file.
// A second scan must not queue the same file twice, and if the publisher's copy
// changed since the first scan the watermark has to go back to zero - the .part
// file holds the prefix of a version that no longer exists.
func (s *Store) EnqueueJob(ctx context.Context, j Job) (int64, error) {
	now := time.Now().UnixMilli()

	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var (
			existing  int64
			total, ms int64
			bytesDone int64
		)
		err := tx.QueryRowContext(ctx,
			`SELECT id, bytes_total, mtime, bytes_done FROM jobs
			 WHERE pair_id = ? AND relpath = ? AND state IN (?, ?, ?)`,
			j.PairID, j.Path, JobPending, JobRunning, JobVerifying).Scan(&existing, &total, &ms, &bytesDone)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			res, err := tx.ExecContext(ctx,
				`INSERT INTO jobs (pair_id, relpath, op, bytes_total, bytes_done, mtime, state, created_at, updated_at)
				 VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?)`,
				j.PairID, j.Path, j.Op, j.BytesTotal, j.MTime.UnixMilli(), JobPending, now, now)
			if err != nil {
				return fmt.Errorf("queue %q: %w", j.Path, err)
			}
			id, err = res.LastInsertId()
			if err != nil {
				return fmt.Errorf("queue %q: %w", j.Path, err)
			}
			return nil

		case err != nil:
			return fmt.Errorf("look up queued job for %q: %w", j.Path, err)
		}

		id = existing
		if total == j.BytesTotal && ms == j.MTime.UnixMilli() {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET op = ?, bytes_total = ?, mtime = ?, bytes_done = 0, state = ?,
			                 next_attempt_at = 0, error = NULL, updated_at = ?
			 WHERE id = ?`,
			j.Op, j.BytesTotal, j.MTime.UnixMilli(), JobPending, now, existing); err != nil {
			return fmt.Errorf("requeue %q: %w", j.Path, err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

const jobColumns = `id, pair_id, relpath, op, bytes_total, bytes_done, mtime, state,
                    attempts, next_attempt_at, error, created_at, updated_at`

// ClaimJob takes the next runnable transfer of a pair and marks it running. The
// claim and the attempt counter move in one statement, so two runners - or a
// runner and a restart - cannot pick up the same file.
//
// The ordering is the index's own, which is what keeps a claim O(1) on a queue
// with thirty thousand rows in it: ordering by id instead costs a sort of every
// pending row, on every file. Fresh work therefore runs before anything that is
// backing off, which is the order to want anyway.
func (s *Store) ClaimJob(ctx context.Context, pairID int64, now time.Time) (Job, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`UPDATE jobs SET state = ?, attempts = attempts + 1, updated_at = ?
		 WHERE id = (SELECT id FROM jobs
		             WHERE pair_id = ? AND state = ? AND next_attempt_at <= ?
		             ORDER BY next_attempt_at, id LIMIT 1)
		 RETURNING `+jobColumns,
		JobRunning, now.UnixMilli(), pairID, JobPending, now.UnixMilli())

	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	return j, err == nil, err
}

// JobProgress persists the resume watermark. It is called as the transfer runs,
// so a restart resumes near where it stopped rather than at the start of the file.
func (s *Store) JobProgress(ctx context.Context, id, bytesDone int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET bytes_done = ?, updated_at = ? WHERE id = ?`,
		bytesDone, time.Now().UnixMilli(), id); err != nil {
		return fmt.Errorf("record progress for job %d: %w", id, err)
	}
	return nil
}

// SetJobState moves a job along its state machine.
func (s *Store) SetJobState(ctx context.Context, id int64, state string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state = ?, error = NULL, updated_at = ? WHERE id = ?`,
		state, time.Now().UnixMilli(), id); err != nil {
		return fmt.Errorf("set job %d to %s: %w", id, state, err)
	}
	return nil
}

// ReleaseJob hands a claimed job back untouched: not a failure, just a run that
// stopped. The attempt is given back too, because a transfer window closing mid
// file must not eat a file's retry budget - a large enough file would otherwise
// exhaust its attempts without ever having failed at anything.
func (s *Store) ReleaseJob(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state = ?, attempts = max(attempts - 1, 0), updated_at = ? WHERE id = ?`,
		JobPending, time.Now().UnixMilli(), id); err != nil {
		return fmt.Errorf("release job %d: %w", id, err)
	}
	return nil
}

// FailJob records an attempt that did not work. It reschedules while the job has
// attempts left and gives up permanently once it does not; retrying returns true
// so the caller can say which happened. bytes_done is left alone - the bytes
// already in the .part file are still good, and re-fetching them is the waste
// the watermark exists to avoid.
func (s *Store) FailJob(ctx context.Context, id int64, cause error, maxAttempts int, backoff time.Duration) (retrying bool, err error) {
	now := time.Now()

	err = s.tx(ctx, func(tx *sql.Tx) error {
		var attempts int
		if err := tx.QueryRowContext(ctx, `SELECT attempts FROM jobs WHERE id = ?`, id).Scan(&attempts); err != nil {
			return fmt.Errorf("fail job %d: %w", id, err)
		}

		state, next := JobFailed, int64(0)
		if attempts < maxAttempts {
			state = JobPending
			next = now.Add(backoffFor(attempts, backoff)).UnixMilli()
			retrying = true
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET state = ?, next_attempt_at = ?, error = ?, updated_at = ? WHERE id = ?`,
			state, next, cause.Error(), now.UnixMilli(), id); err != nil {
			return fmt.Errorf("fail job %d: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return retrying, nil
}

// backoffFor doubles the wait per attempt and stops doubling at an hour: a
// publisher that is off for the night should be retried on the hour, not in
// three weeks.
func backoffFor(attempts int, base time.Duration) time.Duration {
	if base <= 0 {
		base = 30 * time.Second
	}
	d := base
	for i := 1; i < attempts && d < time.Hour; i++ {
		d *= 2
	}
	if d > time.Hour {
		d = time.Hour
	}
	return d
}

// RequeueRunning puts back the jobs a crash, a restart or a self-update left in
// flight. It is the first thing a run does (DESIGN.md §2.4).
func (s *Store) RequeueRunning(ctx context.Context, pairID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state = ?, updated_at = ? WHERE pair_id = ? AND state IN (?, ?)`,
		JobPending, time.Now().UnixMilli(), pairID, JobRunning, JobVerifying)
	if err != nil {
		return 0, fmt.Errorf("requeue running jobs: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// JobQueue is the queue at a glance: how many rows are in each state, and how
// much work is left.
type JobQueue struct {
	Counts       map[string]int64 `json:"counts"`
	PendingBytes int64            `json:"pendingBytes"`
	DoneBytes    int64            `json:"doneBytes"`
	NextAttempt  *time.Time       `json:"nextAttempt,omitempty"`
}

// Queue summarizes a pair's jobs.
func (s *Store) Queue(ctx context.Context, pairID int64) (JobQueue, error) {
	q := JobQueue{Counts: map[string]int64{}}

	rows, err := s.db.QueryContext(ctx,
		`SELECT state, count(*), coalesce(sum(bytes_total - bytes_done), 0), coalesce(sum(bytes_total), 0)
		 FROM jobs WHERE pair_id = ? GROUP BY state`, pairID)
	if err != nil {
		return q, fmt.Errorf("read job queue: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			state            string
			n, remain, total int64
		)
		if err := rows.Scan(&state, &n, &remain, &total); err != nil {
			return q, fmt.Errorf("scan job queue: %w", err)
		}
		q.Counts[state] = n
		switch state {
		case JobPending, JobRunning, JobVerifying:
			q.PendingBytes += remain
		case JobDone:
			// The whole file, not the watermark: a job that finished in place - the
			// destination was already correct - never moved its watermark at all.
			q.DoneBytes += total
		}
	}
	if err := rows.Err(); err != nil {
		return q, err
	}

	var next sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT min(next_attempt_at) FROM jobs WHERE pair_id = ? AND state = ? AND next_attempt_at > 0`,
		pairID, JobPending).Scan(&next); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return q, fmt.Errorf("read next attempt: %w", err)
	}
	if next.Valid {
		t := time.UnixMilli(next.Int64)
		q.NextAttempt = &t
	}
	return q, nil
}

// JobFilter selects a slice of the queue.
type JobFilter struct {
	PairID *int64
	States []string
	Limit  int
}

// Jobs returns matching jobs, oldest first - which is the order they run in.
func (s *Store) Jobs(ctx context.Context, f JobFilter) ([]Job, error) {
	var (
		where []string
		args  []any
	)
	if f.PairID != nil {
		where = append(where, "pair_id = ?")
		args = append(args, *f.PairID)
	}
	if len(f.States) > 0 {
		where = append(where, "state IN ("+placeholders(len(f.States))+")")
		for _, st := range f.States {
			args = append(args, st)
		}
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}

	q := `SELECT ` + jobColumns + ` FROM jobs`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("read jobs: %w", err)
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// PruneJobs drops finished jobs older than the retention window. Failed ones stay
// until they are looked at.
func (s *Store) PruneJobs(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM jobs WHERE state = ? AND updated_at < ?`, JobDone, olderThan.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("prune jobs: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func scanJob(row rowScanner) (Job, error) {
	var (
		j                Job
		mtime, next      int64
		created, updated int64
		errMsg           sql.NullString
	)
	if err := row.Scan(&j.ID, &j.PairID, &j.Path, &j.Op, &j.BytesTotal, &j.BytesDone, &mtime,
		&j.State, &j.Attempts, &next, &errMsg, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, err
		}
		return Job{}, fmt.Errorf("scan job: %w", err)
	}
	j.MTime = time.UnixMilli(mtime)
	if next > 0 {
		j.NextAttemptAt = time.UnixMilli(next)
	}
	j.Error = errMsg.String
	j.CreatedAt, j.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return j, nil
}
