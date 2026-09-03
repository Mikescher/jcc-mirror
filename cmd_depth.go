package main

import (
	"context"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/logs"
)

// cmdDepth is the other half of M0 step 3: does the server honour
// Depth: infinity? RFC 4918 lets it refuse, and DSM is expected to. The answer
// decides whether a scan is one request per directory or one for the whole tree,
// which is a difference of thousands of round trips (DESIGN.md §2.1).
func cmdDepth(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("depth")
	dir := fs.String("path", "", "remote directory to probe - point this at a SMALL subdirectory first, an honoured infinity request on the collection root returns the entire tree in one XML document")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := &logs.Logger{Verbose: cfg.verbose}
	client, _, closeFn, err := cfg.openBoth(ctx, logger)
	if err != nil {
		return err
	}
	defer closeFn()

	honoured, reason, err := client.ProbeDepthInfinity(ctx, *dir)
	if err != nil {
		return err
	}
	if !honoured {
		logger.Infof("depth infinity on %q: refused (%s)", *dir, reason)
		logger.Infof("=> the scanner walks one PROPFIND per directory, as the design assumes")
		return nil
	}

	logger.Infof("depth infinity on %q: honoured (%s)", *dir, reason)

	start := time.Now()
	entries, err := client.Propfind(ctx, *dir, "infinity", nil)
	if err != nil {
		return err
	}
	logger.Infof("=> %d entries in one request, %s - worth using where the subtree is small enough to hold in memory",
		len(entries), format.Duration(time.Since(start)))
	return nil
}
