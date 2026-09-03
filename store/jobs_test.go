package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

var jobTime = time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC)

func enqueue(t *testing.T, s *Store, pairID int64, path string, size int64, mtime time.Time) int64 {
	t.Helper()
	id, err := s.EnqueueJob(context.Background(), Job{
		PairID: pairID, Path: path, Op: OpAdd, BytesTotal: size, MTime: mtime,
	})
	if err != nil {
		t.Fatalf("EnqueueJob(%q): %v", path, err)
	}
	return id
}

func jobByID(t *testing.T, s *Store, id int64) Job {
	t.Helper()
	jobs, err := s.Jobs(context.Background(), JobFilter{})
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	for _, j := range jobs {
		if j.ID == id {
			return j
		}
	}
	t.Fatalf("no job with id %d", id)
	return Job{}
}

func TestEnqueueJobQueuesAFileOnce(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	id := enqueue(t, s, p.ID, "a.mkv", 1000, jobTime)
	if err := s.JobProgress(ctx, id, 400); err != nil {
		t.Fatalf("JobProgress: %v", err)
	}

	again := enqueue(t, s, p.ID, "a.mkv", 1000, jobTime)
	if again != id {
		t.Fatalf("a second scan queued a.mkv as job %d as well as %d", again, id)
	}

	jobs, err := s.Jobs(ctx, JobFilter{PairID: &p.ID})
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("queue holds %d jobs for one file", len(jobs))
	}
	// The publisher's copy did not move, so the .part file on disk is still the
	// prefix of the file being fetched: re-fetching those bytes is pure waste.
	if jobs[0].BytesDone != 400 {
		t.Errorf("bytes_done = %d after an unchanged re-enqueue, want 400", jobs[0].BytesDone)
	}
}

// A file that changed on the publisher invalidates the .part file: it holds the
// prefix of a version that no longer exists, so the watermark has to go to zero.
func TestEnqueueJobResetsTheWatermarkWhenTheFileChanged(t *testing.T) {
	cases := map[string]struct {
		size  int64
		mtime time.Time
	}{
		"the copy grew":          {2000, jobTime},
		"the copy was rewritten": {1000, jobTime.Add(time.Minute)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := newStore(t)
			p := newPair(t, s, "media")

			id := enqueue(t, s, p.ID, "a.mkv", 1000, jobTime)
			if err := s.JobProgress(ctx, id, 400); err != nil {
				t.Fatalf("JobProgress: %v", err)
			}

			if again := enqueue(t, s, p.ID, "a.mkv", c.size, c.mtime); again != id {
				t.Fatalf("re-enqueue returned job %d, want %d", again, id)
			}

			j := jobByID(t, s, id)
			if j.BytesDone != 0 {
				t.Errorf("bytes_done = %d, want 0", j.BytesDone)
			}
			if j.BytesTotal != c.size {
				t.Errorf("bytes_total = %d, want %d", j.BytesTotal, c.size)
			}
			if !j.MTime.Equal(c.mtime) {
				t.Errorf("mtime = %v, want %v", j.MTime, c.mtime)
			}
			if j.State != JobPending || !j.NextAttemptAt.IsZero() {
				t.Errorf("state/next attempt = %q/%v, want a job ready to run", j.State, j.NextAttemptAt)
			}
		})
	}
}

// The open-job index covers pending, running and verifying only, so a file that
// is transferred again after a completed one gets a row of its own.
func TestEnqueueJobAfterADoneTransfer(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	first := enqueue(t, s, p.ID, "a.mkv", 1000, jobTime)
	if err := s.SetJobState(ctx, first, JobDone); err != nil {
		t.Fatalf("SetJobState: %v", err)
	}

	second := enqueue(t, s, p.ID, "a.mkv", 2000, jobTime)
	if second == first {
		t.Fatalf("re-queueing after a completed transfer reused job %d", first)
	}
	jobs, err := s.Jobs(ctx, JobFilter{PairID: &p.ID})
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Errorf("queue holds %d jobs, want 2", len(jobs))
	}
}

func TestClaimJob(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")
	now := time.Now()

	if _, ok, err := s.ClaimJob(ctx, p.ID, now); err != nil || ok {
		t.Fatalf("ClaimJob on an empty queue = %v (err %v), want nothing", ok, err)
	}

	first := enqueue(t, s, p.ID, "a.mkv", 1000, jobTime)
	second := enqueue(t, s, p.ID, "b.mkv", 500, jobTime)

	j, ok, err := s.ClaimJob(ctx, p.ID, now)
	if err != nil || !ok {
		t.Fatalf("ClaimJob: %v (found %v)", err, ok)
	}
	if j.ID != first {
		t.Errorf("claimed job %d, want the oldest (%d)", j.ID, first)
	}
	if j.State != JobRunning || j.Attempts != 1 {
		t.Errorf("claimed job is %q with %d attempts, want running with 1", j.State, j.Attempts)
	}
	if j.Path != "a.mkv" || j.BytesTotal != 1000 || !j.MTime.Equal(jobTime) {
		t.Errorf("claimed job = %+v", j)
	}

	if j, ok, err = s.ClaimJob(ctx, p.ID, now); err != nil || !ok || j.ID != second {
		t.Fatalf("second claim = job %d (found %v, err %v), want %d", j.ID, ok, err, second)
	}
	if _, ok, err := s.ClaimJob(ctx, p.ID, now); err != nil || ok {
		t.Errorf("a third claim took %v from a drained queue (err %v)", ok, err)
	}
}

func TestClaimJobWaitsOutTheBackoff(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")
	now := time.Now()

	id := enqueue(t, s, p.ID, "a.mkv", 1000, jobTime)
	if _, ok, err := s.ClaimJob(ctx, p.ID, now); err != nil || !ok {
		t.Fatalf("ClaimJob: %v (found %v)", err, ok)
	}

	retrying, err := s.FailJob(ctx, id, errors.New("connection reset by peer"), 5, time.Minute)
	if err != nil {
		t.Fatalf("FailJob: %v", err)
	}
	if !retrying {
		t.Fatal("FailJob gave up on the first of five attempts")
	}

	if _, ok, err := s.ClaimJob(ctx, p.ID, now); err != nil || ok {
		t.Errorf("claimed a job that is still inside its backoff window (err %v)", err)
	}

	j, ok, err := s.ClaimJob(ctx, p.ID, now.Add(2*time.Minute))
	if err != nil || !ok {
		t.Fatalf("ClaimJob after the backoff: %v (found %v)", err, ok)
	}
	if j.ID != id || j.Attempts != 2 {
		t.Errorf("re-claimed job %d with %d attempts, want %d with 2", j.ID, j.Attempts, id)
	}
}

func TestFailJobGivesUpOnTheLastAttempt(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")
	now := time.Now()

	id := enqueue(t, s, p.ID, "a.mkv", 1000, jobTime)
	if err := s.JobProgress(ctx, id, 400); err != nil {
		t.Fatalf("JobProgress: %v", err)
	}

	const maxAttempts = 2
	if _, ok, err := s.ClaimJob(ctx, p.ID, now); err != nil || !ok {
		t.Fatalf("ClaimJob: %v (found %v)", err, ok)
	}
	retrying, err := s.FailJob(ctx, id, errors.New("connection reset by peer"), maxAttempts, time.Minute)
	if err != nil {
		t.Fatalf("FailJob: %v", err)
	}
	if !retrying {
		t.Fatal("FailJob gave up on attempt 1 of 2")
	}
	if j := jobByID(t, s, id); j.State != JobPending || j.NextAttemptAt.IsZero() {
		t.Errorf("after a retriable failure the job is %q, next attempt %v", j.State, j.NextAttemptAt)
	}

	if _, ok, err := s.ClaimJob(ctx, p.ID, now.Add(2*time.Minute)); err != nil || !ok {
		t.Fatalf("ClaimJob: %v (found %v)", err, ok)
	}
	retrying, err = s.FailJob(ctx, id, errors.New("disk full"), maxAttempts, time.Minute)
	if err != nil {
		t.Fatalf("FailJob: %v", err)
	}
	if retrying {
		t.Fatal("FailJob rescheduled a job that has used all its attempts")
	}

	j := jobByID(t, s, id)
	if j.State != JobFailed {
		t.Errorf("state = %q, want %q", j.State, JobFailed)
	}
	if !j.NextAttemptAt.IsZero() {
		t.Errorf("a failed job is scheduled for %v, want no next attempt", j.NextAttemptAt)
	}
	if j.Error != "disk full" {
		t.Errorf("error = %q, want the cause", j.Error)
	}
	// The bytes already in the .part file are still good; a failure must not throw
	// away the watermark that exists to avoid re-fetching them.
	if j.BytesDone != 400 {
		t.Errorf("bytes_done = %d after a failure, want 400", j.BytesDone)
	}
}

func TestBackoffFor(t *testing.T) {
	cases := []struct {
		attempts int
		base     time.Duration
		want     time.Duration
	}{
		{1, 30 * time.Second, 30 * time.Second},
		{2, 30 * time.Second, time.Minute},
		{3, 30 * time.Second, 2 * time.Minute},
		{4, 30 * time.Second, 4 * time.Minute},
		// A publisher that is off for the night is retried on the hour, not in
		// three weeks: the doubling stops at an hour.
		{8, 30 * time.Second, time.Hour},
		{100, 30 * time.Second, time.Hour},
		{2, 45 * time.Minute, time.Hour},
		{1, 2 * time.Hour, time.Hour},
		{1, 0, 30 * time.Second},
		{3, 0, 2 * time.Minute},
	}
	for _, c := range cases {
		if got := backoffFor(c.attempts, c.base); got != c.want {
			t.Errorf("backoffFor(%d, %v) = %v, want %v", c.attempts, c.base, got, c.want)
		}
	}
}

// A transfer window closing mid file is not a failure, so it must not eat a
// file's retry budget.
func TestReleaseJobGivesTheAttemptBack(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")
	now := time.Now()

	id := enqueue(t, s, p.ID, "a.mkv", 1000, jobTime)
	if _, ok, err := s.ClaimJob(ctx, p.ID, now); err != nil || !ok {
		t.Fatalf("ClaimJob: %v (found %v)", err, ok)
	}
	if err := s.ReleaseJob(ctx, id); err != nil {
		t.Fatalf("ReleaseJob: %v", err)
	}

	j := jobByID(t, s, id)
	if j.State != JobPending || j.Attempts != 0 {
		t.Errorf("released job is %q with %d attempts, want pending with 0", j.State, j.Attempts)
	}
	if _, ok, err := s.ClaimJob(ctx, p.ID, now); err != nil || !ok {
		t.Errorf("a released job was not claimable again (found %v, err %v)", ok, err)
	}
}

// A crash, a restart or a self-update leaves rows in flight; requeueing them is
// what makes a transfer retriable at all (DESIGN.md §2.4).
func TestRequeueRunning(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")
	other := newPair(t, s, "jcc")
	now := time.Now()

	running := enqueue(t, s, p.ID, "a.mkv", 1000, jobTime)
	verifying := enqueue(t, s, p.ID, "b.mkv", 500, jobTime)
	enqueue(t, s, p.ID, "c.mkv", 300, jobTime)
	elsewhere := enqueue(t, s, other.ID, "d.mkv", 100, jobTime)

	for i := 0; i < 2; i++ {
		if _, ok, err := s.ClaimJob(ctx, p.ID, now); err != nil || !ok {
			t.Fatalf("ClaimJob: %v (found %v)", err, ok)
		}
	}
	if _, ok, err := s.ClaimJob(ctx, other.ID, now); err != nil || !ok {
		t.Fatalf("ClaimJob: %v (found %v)", err, ok)
	}
	if err := s.SetJobState(ctx, verifying, JobVerifying); err != nil {
		t.Fatalf("SetJobState: %v", err)
	}

	n, err := s.RequeueRunning(ctx, p.ID)
	if err != nil {
		t.Fatalf("RequeueRunning: %v", err)
	}
	if n != 2 {
		t.Errorf("requeued %d jobs, want the running and the verifying one", n)
	}
	for _, id := range []int64{running, verifying} {
		if j := jobByID(t, s, id); j.State != JobPending {
			t.Errorf("job %d is %q after the requeue, want pending", id, j.State)
		}
	}
	if j := jobByID(t, s, elsewhere); j.State != JobRunning {
		t.Errorf("requeueing one pair touched another pair's job (now %q)", j.State)
	}
}

func TestQueue(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")
	now := time.Now()

	running := enqueue(t, s, p.ID, "a.mkv", 1000, jobTime)
	pending := enqueue(t, s, p.ID, "b.mkv", 500, jobTime)
	done := enqueue(t, s, p.ID, "c.mkv", 300, jobTime)

	if _, ok, err := s.ClaimJob(ctx, p.ID, now); err != nil || !ok {
		t.Fatalf("ClaimJob: %v (found %v)", err, ok)
	}
	if err := s.JobProgress(ctx, running, 200); err != nil {
		t.Fatalf("JobProgress: %v", err)
	}
	if err := s.JobProgress(ctx, done, 300); err != nil {
		t.Fatalf("JobProgress: %v", err)
	}
	if err := s.SetJobState(ctx, done, JobDone); err != nil {
		t.Fatalf("SetJobState: %v", err)
	}

	q, err := s.Queue(ctx, p.ID)
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	for state, want := range map[string]int64{JobPending: 1, JobRunning: 1, JobDone: 1} {
		if q.Counts[state] != want {
			t.Errorf("counts[%s] = %d, want %d", state, q.Counts[state], want)
		}
	}
	if q.PendingBytes != 1300 {
		t.Errorf("pending bytes = %d, want 1300 (800 left of the running file plus 500)", q.PendingBytes)
	}
	if q.NextAttempt != nil {
		t.Errorf("next attempt = %v with nothing in backoff", q.NextAttempt)
	}

	if _, err := s.FailJob(ctx, pending, errors.New("no route to host"), 5, time.Minute); err != nil {
		t.Fatalf("FailJob: %v", err)
	}
	q, err = s.Queue(ctx, p.ID)
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if q.NextAttempt == nil || !q.NextAttempt.After(now) {
		t.Errorf("next attempt = %v, want the backoff of the failed job", q.NextAttempt)
	}
}

func TestQueueDoneBytes(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	id := enqueue(t, s, p.ID, "a.mkv", 1000, jobTime)
	if err := s.JobProgress(ctx, id, 1000); err != nil {
		t.Fatalf("JobProgress: %v", err)
	}
	if err := s.SetJobState(ctx, id, JobDone); err != nil {
		t.Fatalf("SetJobState: %v", err)
	}
	enqueue(t, s, p.ID, "b.mkv", 500, jobTime)

	q, err := s.Queue(ctx, p.ID)
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if q.DoneBytes != 1000 {
		t.Errorf("done bytes = %d, want 1000", q.DoneBytes)
	}
}

func TestJobsFilter(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")
	other := newPair(t, s, "jcc")

	first := enqueue(t, s, p.ID, "a.mkv", 1000, jobTime)
	second := enqueue(t, s, p.ID, "b.mkv", 500, jobTime)
	enqueue(t, s, other.ID, "c.mkv", 300, jobTime)
	if err := s.SetJobState(ctx, second, JobFailed); err != nil {
		t.Fatalf("SetJobState: %v", err)
	}

	all, err := s.Jobs(ctx, JobFilter{})
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(all) != 3 || all[0].ID != first {
		t.Fatalf("Jobs returned %d rows starting at %d, want 3 oldest first", len(all), all[0].ID)
	}

	mine, err := s.Jobs(ctx, JobFilter{PairID: &p.ID})
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(mine) != 2 {
		t.Errorf("pair filter returned %d jobs, want 2", len(mine))
	}

	failed, err := s.Jobs(ctx, JobFilter{States: []string{JobFailed}})
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(failed) != 1 || failed[0].ID != second {
		t.Errorf("state filter returned %d jobs", len(failed))
	}

	limited, err := s.Jobs(ctx, JobFilter{Limit: 1})
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("limit 1 returned %d jobs", len(limited))
	}
}

func TestPruneJobsKeepsFailures(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := newPair(t, s, "media")

	done := enqueue(t, s, p.ID, "a.mkv", 1000, jobTime)
	failed := enqueue(t, s, p.ID, "b.mkv", 500, jobTime)
	enqueue(t, s, p.ID, "c.mkv", 300, jobTime)
	if err := s.SetJobState(ctx, done, JobDone); err != nil {
		t.Fatalf("SetJobState: %v", err)
	}
	if err := s.SetJobState(ctx, failed, JobFailed); err != nil {
		t.Fatalf("SetJobState: %v", err)
	}

	n, err := s.PruneJobs(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("PruneJobs: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d jobs, want only the completed one", n)
	}
	// A failed job stays until someone has looked at it.
	if j := jobByID(t, s, failed); j.State != JobFailed {
		t.Errorf("failed job is %q after the prune", j.State)
	}
}
