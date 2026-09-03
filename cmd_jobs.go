package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/store"
)

// jobStates are the states a job can be in, in the order a transfer moves
// through them.
var jobStates = []string{store.JobPending, store.JobRunning, store.JobVerifying, store.JobDone, store.JobFailed}

// cmdJobs is how a failed transfer is looked at. Every file is a row with its own
// attempt counter, backoff and last error, which is what makes a transfer
// retriable rather than an in-memory loop that dies with the process
// (DESIGN.md §2.4) - and this is where those rows are read.
//
// It touches the store and nothing else: a queue can be inspected with the
// publisher switched off.
func cmdJobs(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("jobs")
	ref := fs.String("pair", "", "the pair whose queue to look at, by name or id")
	limit := fs.Int("limit", 25, "how many jobs to list")
	var states []string
	fs.Var(&listFlag{dst: &states}, "state",
		"only jobs in this state: pending, running, verifying, done or failed; repeat the flag, or separate several with commas")
	if err := fs.Parse(args); err != nil {
		return err
	}

	for _, s := range states {
		if !contains(jobStates, s) {
			return fmt.Errorf("unknown job state %q: want one of %s", s, strings.Join(jobStates, ", "))
		}
	}

	st, err := cfg.openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	pair, err := openPair(ctx, st, pickRef(*ref, first(fs.Args())))
	if err != nil {
		return err
	}

	queue, err := st.Queue(ctx, pair.ID)
	if err != nil {
		return err
	}
	fmt.Printf("\nqueue of %q: %s\n", pair.Name, queueLine(queue))

	jobs, err := st.Jobs(ctx, store.JobFilter{PairID: &pair.ID, States: states, Limit: *limit})
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		fmt.Printf("\nno jobs match\n")
		return nil
	}

	values, err := st.Config(ctx)
	if err != nil {
		return err
	}
	maxAttempts := values.Int(store.KeyMaxAttempts)

	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprint(w, "  id\tstate\ttry\tdone\ttotal\tnext\tpath\n")
	for _, j := range jobs {
		fmt.Fprintf(w, "  %d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			j.ID, j.State, attemptsText(j, maxAttempts), format.Bytes(j.BytesDone), format.Bytes(j.BytesTotal),
			nextAttemptText(j), j.Path)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// The error is what the command exists for, and it does not fit a column.
	var failures int
	for _, j := range jobs {
		if j.Error == "" {
			continue
		}
		if failures == 0 {
			fmt.Println()
		}
		failures++
		fmt.Printf("  #%d %s\n      %s\n", j.ID, j.Path, j.Error)
	}

	if int64(len(jobs)) < totalJobs(queue) {
		fmt.Printf("\n%s of %s jobs listed; -limit and -state pick out the rest\n",
			format.Comma(int64(len(jobs))), format.Comma(totalJobs(queue)))
	}
	return nil
}

// queueLine is the one-line state of a queue, shared with `pairs`.
func queueLine(q store.JobQueue) string {
	var parts []string
	for _, state := range jobStates {
		if n := q.Counts[state]; n > 0 {
			parts = append(parts, format.Comma(n)+" "+state)
		}
	}
	if len(parts) == 0 {
		return "empty"
	}

	line := strings.Join(parts, ", ")
	if q.PendingBytes > 0 {
		line += fmt.Sprintf(" (%s still to move)", format.Bytes(q.PendingBytes))
	}
	if q.NextAttempt != nil {
		line += ", next attempt " + untilText(*q.NextAttempt)
	}
	return line
}

func totalJobs(q store.JobQueue) int64 {
	var n int64
	for _, c := range q.Counts {
		n += c
	}
	return n
}

func attemptsText(j store.Job, maxAttempts int) string {
	if maxAttempts > 0 {
		return fmt.Sprintf("%d/%d", j.Attempts, maxAttempts)
	}
	return fmt.Sprintf("%d", j.Attempts)
}

// nextAttemptText renders the backoff a job is sitting out. Only a job waiting on
// one has a next attempt at all; the rest run as soon as the queue reaches them.
func nextAttemptText(j store.Job) string {
	if j.NextAttemptAt.IsZero() {
		return "-"
	}
	return untilText(j.NextAttemptAt)
}

// untilText renders a due time as the wait that is left, which is what someone
// watching a backing-off queue actually wants to know.
func untilText(t time.Time) string {
	if d := time.Until(t); d > 0 {
		return "in " + format.Duration(d)
	}
	return "now"
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
