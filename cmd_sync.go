package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"blackforestbytes.com/jcc-mirror/engine"
	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/logs"
)

// planOnlySample is how many entries -plan-only lists. `plan` is the command for
// looking at a diff in detail; here the numbers are what matter.
const planOnlySample = 25

// cmdSync moves the bytes: it turns the diff into rows in the job queue and then
// works the queue off, one file at a time with several ranged streams inside each
// (DESIGN.md §2.4).
//
// Interrupting it is routine and costs nothing. The claimed job goes back to
// pending with its resume watermark intact, so a 40 GB file stopped at 39 GB
// carries on from 39 GB in the next run - which is also what a closed transfer
// window looks like to the scheduler, and what a self-update will look like in
// M7.
func cmdSync(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("sync")
	ref := fs.String("pair", "", "the pair to transfer, by name or id")
	planOnly := fs.Bool("plan-only", false, "print what would happen and stop: nothing is queued and no byte is moved")
	progress := fs.Duration("progress", 5*time.Second, "how often to report where the transfer is, 0 to disable")
	fs.StringVar(&cfg.limit, "limit", "", "bandwidth cap, e.g. 5MiB or off; the schedule's current cap is used when this is left out")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := &logs.Logger{Verbose: cfg.verbose}
	eng, st, closeFn, err := cfg.openEngine(ctx, logger)
	if err != nil {
		return err
	}
	defer closeFn()

	pair, err := openPair(ctx, st, pickRef(*ref, first(fs.Args())))
	if err != nil {
		return err
	}

	if *planOnly {
		plan, err := eng.Plan(ctx, pair, planOnlySample)
		if err != nil {
			return err
		}
		return printPlan(plan)
	}

	plan, err := eng.Enqueue(ctx, pair)
	if err != nil {
		return err
	}
	logger.Infof("sync: %s; %s file(s) queued", plan.String(), format.Comma(int64(plan.Queued)))

	stop := reportSync(ctx, logger, eng, *progress)
	res, syncErr := eng.Sync(ctx, pair)
	stop()

	// A plan refused for want of room never started, so there is nothing to
	// summarize - only the reason (DESIGN.md §2.6).
	var noSpace *engine.SpaceError
	if errors.As(syncErr, &noSpace) {
		return syncErr
	}
	printSync(res)

	if syncErr != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("sync interrupted after %s; the queue is left where it was and the next run continues it", format.Duration(res.Duration))
		}
		return syncErr
	}
	if res.Failed > 0 {
		return fmt.Errorf("%s file(s) failed for good; `jcc-mirror jobs -pair %s -state failed` says why", format.Comma(int64(res.Failed)), pair.Name)
	}
	return nil
}

// reportSync prints where the run is every interval until the returned function
// is called. It quotes the instantaneous rate beside the average because a
// transfer that has degraded looks fine on the average for a long time.
//
// The ETA is the run's remaining bytes at the run's average rate. A file found
// already correct on disk costs no bytes and shortens the queue anyway, so the
// estimate errs on the pessimistic side rather than the flattering one.
func reportSync(ctx context.Context, log *logs.Logger, eng *engine.Engine, interval time.Duration) func() {
	if interval <= 0 {
		return func() {}
	}

	stop := make(chan struct{})
	var once sync.Once

	go func() {
		tick := time.NewTicker(interval)
		defer tick.Stop()

		lastAt, lastBytes := time.Now(), int64(0)

		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case now := <-tick.C:
				p := eng.Progress()
				if p.StartedAt.IsZero() {
					// Between runs, or waiting out a backoff: there is nothing to say.
					continue
				}

				inst := format.Rate(p.RunBytes-lastBytes, now.Sub(lastAt))
				avg := format.Rate(p.RunBytes, now.Sub(p.StartedAt))
				lastAt, lastBytes = now, p.RunBytes

				log.Infof("sync: %s, run %s of %s, %s now, %s avg, eta %s",
					progressWhere(p), format.Bytes(p.RunBytes), format.Bytes(p.RunTotal), inst, avg, progressETA(p, now))
			}
		}
	}()

	return func() { once.Do(func() { close(stop) }) }
}

// progressWhere is the file half of a progress line: which file of how many, and
// how far into it the transfer is.
func progressWhere(p engine.Progress) string {
	where := fmt.Sprintf("%s/%s files", format.Comma(int64(p.Files)), format.Comma(int64(p.FilesTotal)))
	if p.Path == "" {
		return where
	}
	where += fmt.Sprintf(", %s %s/%s", p.Path, format.Bytes(p.FileDone), format.Bytes(p.FileTotal))
	if p.FileTotal > 0 {
		where += fmt.Sprintf(" (%.0f%%)", 100*float64(p.FileDone)/float64(p.FileTotal))
	}
	return where
}

func progressETA(p engine.Progress, now time.Time) string {
	if p.RunBytes <= 0 || p.RunTotal <= p.RunBytes {
		return "-"
	}
	remaining := float64(p.RunTotal-p.RunBytes) / float64(p.RunBytes) * float64(now.Sub(p.StartedAt))
	return format.Duration(time.Duration(remaining))
}

func printSync(res engine.SyncResult) {
	fmt.Printf("\nsync of %q ended after %s\n", res.PairName, format.Duration(res.Duration))
	fmt.Printf("  transferred    %s files, %s\n", format.Comma(int64(res.Files)), format.Bytes(res.Bytes))
	fmt.Printf("  already here   %s files found correct on disk\n", format.Comma(int64(res.InPlace)))
	fmt.Printf("  retrying       %s files failed this run and are queued again\n", format.Comma(int64(res.Retrying)))
	fmt.Printf("  failed         %s files out of attempts\n", format.Comma(int64(res.Failed)))
	fmt.Printf("  still queued   %s files\n", format.Comma(res.Pending))
	fmt.Printf("  average        %s\n", format.Rate(res.Bytes, res.Duration))

	switch {
	case res.Failed > 0:
		fmt.Printf("\n=> `jcc-mirror jobs -pair %s -state failed` shows what went wrong with each of them.\n", res.PairName)
	case res.Pending > 0:
		fmt.Printf("\n=> %s file(s) are still queued; run sync again to carry on where this stopped.\n", format.Comma(res.Pending))
	}
}
