package main

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/webdav"
)

// cmdSoak is the last part of M0 step 5: leave a transfer running for hours and
// see whether DSM's WebDAV holds up. A share that works perfectly for ninety
// seconds and drops every connection at the ten-minute mark passes every other
// command here, and would first show up with a 30 TB sync underway.
//
// Reconnecting is done the way the transfer engine will do it - reopen at the
// byte offset reached, never from the start - so the reconnect path is under test
// as much as the connection is.
func cmdSoak(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("soak")
	path := fs.String("path", "", "remote file, relative to -url")
	duration := fs.Duration("duration", 4*time.Hour, "how long to keep going")
	report := fs.Duration("report", time.Minute, "progress interval")
	stallWarn := fs.Duration("stall-warn", 30*time.Second, "warn when no byte arrives for this long")
	backoff := fs.Duration("backoff", 5*time.Second, "delay before reconnecting after an error")
	rewind := fs.Bool("rewind", true, "start over at byte 0 on EOF instead of stopping")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return fmt.Errorf("missing -path")
	}

	logger := &logs.Logger{Verbose: cfg.verbose}
	client, _, closeFn, err := cfg.openBoth(ctx, logger)
	if err != nil {
		return err
	}
	defer closeFn()

	entry, err := client.Stat(ctx, *path)
	if err != nil {
		return err
	}
	logger.Infof("soak: %s, %s, for %s", entry.Path, format.Bytes(entry.Size), format.Duration(*duration))

	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()

	var done, last atomic.Int64
	last.Store(time.Now().UnixNano())

	stopReport := reportBytes(ctx, logger, "soak", &done, 0, *report)
	defer stopReport()
	longestStall := watchStalls(ctx, logger, &last, *stallWarn)

	var (
		start       = time.Now()
		offset      int64
		errCount    int
		reconnects  int
		passes      int
		lastPassLog time.Time
		firstErrAt  time.Duration
		firstErrSet bool
	)

loop:
	for ctx.Err() == nil {
		n, err := streamFrom(ctx, client, *path, offset, &done, &last)
		offset += n

		switch {
		case err == nil:
			passes++
			// A small file against a fast server wraps around several times a
			// second; the progress line already carries the throughput.
			if time.Since(lastPassLog) >= *report {
				logger.Infof("soak: reached the end of the file after %s (pass %d)", format.Bytes(offset), passes)
				lastPassLog = time.Now()
			}
			if !*rewind {
				break loop
			}
			offset = 0

		case ctx.Err() != nil:
			break loop

		default:
			errCount++
			if !firstErrSet {
				firstErrAt, firstErrSet = time.Since(start), true
			}
			logger.Errorf("soak: at offset %d: %v", offset, err)
			if !sleepCtx(ctx, *backoff) {
				break loop
			}
			reconnects++
			logger.Infof("soak: reconnecting at offset %s", format.Bytes(offset))
		}
	}

	elapsed := time.Since(start)
	stopReport()

	fmt.Printf("\nsoak of %q finished after %s\n", entry.Path, format.Duration(elapsed))
	fmt.Printf("  transferred    %s\n", format.Bytes(done.Load()))
	fmt.Printf("  average rate   %s\n", format.Rate(done.Load(), elapsed))
	fmt.Printf("  full passes    %d\n", passes)
	fmt.Printf("  errors         %d\n", errCount)
	fmt.Printf("  reconnects     %d\n", reconnects)
	fmt.Printf("  longest stall  %s\n", format.Duration(time.Duration(longestStall.Load())))

	if firstErrSet {
		fmt.Printf("  first error at %s\n", format.Duration(firstErrAt))
		fmt.Printf("\n=> the server does drop long transfers, so the job state machine has to treat that as routine rather than exceptional (DESIGN.md §2.4)\n")
		return nil
	}
	fmt.Printf("\n=> no interruptions in %s; long ranged GETs are safe against this server\n", format.Duration(elapsed))
	return nil
}

// streamFrom reads a file from offset to its end, discarding the bytes, and
// returns how many arrived before it stopped.
func streamFrom(ctx context.Context, client *webdav.Client, path string, offset int64, done, last *atomic.Int64) (int64, error) {
	body, _, err := client.OpenRange(ctx, path, offset, 0)
	if err != nil {
		return 0, err
	}
	defer body.Close()

	return io.Copy(io.Discard, countingReader{r: body, bytes: done, last: last})
}

// watchStalls records the longest gap between arriving bytes, and warns about one
// while it is happening. A server that stops sending without closing the
// connection produces no error at all; the gap is the only evidence.
func watchStalls(ctx context.Context, logger *logs.Logger, last *atomic.Int64, warn time.Duration) *atomic.Int64 {
	longest := &atomic.Int64{}
	if warn <= 0 {
		return longest
	}

	go func() {
		tick := time.NewTicker(warn / 2)
		defer tick.Stop()

		warned := false
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tick.C:
				gap := now.Sub(time.Unix(0, last.Load()))
				if gap.Nanoseconds() > longest.Load() {
					longest.Store(gap.Nanoseconds())
				}
				switch {
				case gap >= warn && !warned:
					logger.Errorf("soak: no data for %s", format.Duration(gap))
					warned = true
				case gap < warn:
					warned = false
				}
			}
		}
	}()

	return longest
}
