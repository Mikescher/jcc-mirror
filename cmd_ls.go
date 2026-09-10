package main

import (
	"context"
	"fmt"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/remote"
)

// cmdLs is M0 step 3: one directory listing against the real share. It is the
// first thing that touches DSM, and how long a single listing takes is what the
// walk of step 4 costs per directory.
//
// It runs against remote.Remote rather than the SMB client, so -remote-dir lists
// the local stand-in with the same code and the same output.
func cmdLs(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("ls")
	dir := fs.String("path", "", "remote directory, relative to the remote root")
	limit := fs.Int("limit", 200, "print at most this many entries, 0 for all")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := &logs.Logger{Verbose: cfg.verbose}
	rem, closeFn, err := cfg.openEngineRemote(ctx, logger)
	if err != nil {
		return err
	}
	defer closeFn()

	start := time.Now()
	entries, err := rem.List(ctx, *dir)
	elapsed := time.Since(start)
	if err != nil {
		return err
	}

	printEntries(entries, *limit)
	logger.Infof("list %q: %d entries in %s", *dir, len(entries), format.Duration(elapsed))

	// Only the real client counts these, and only it can: the fake reads a local
	// tree that was never anybody's source.
	if counter, ok := rem.(interface{ NonNFCNames() int64 }); ok {
		if n := counter.NonNFCNames(); n > 0 {
			logger.Infof("note: %d name(s) were not NFC - the diff must normalize (DESIGN.md §2.3)", n)
		}
	}
	return nil
}

// printEntries writes the listing as a fixed-width table.
func printEntries(entries []remote.Entry, limit int) {
	var (
		dirs, files int
		bytesTotal  int64
	)
	for i, e := range entries {
		if e.IsDir {
			dirs++
		} else {
			files++
			bytesTotal += e.Size
		}
		if limit > 0 && i >= limit {
			continue
		}

		kind, size := "file", format.Bytes(e.Size)
		if e.IsDir {
			kind, size = "dir", "-"
		}
		fmt.Printf("%-4s  %12s  %s  %s\n", kind, size, e.MTime.Format(time.RFC3339), e.Path)
	}

	if limit > 0 && len(entries) > limit {
		fmt.Printf("... %d more (raise -limit)\n", len(entries)-limit)
	}
	fmt.Printf("\n%d dirs, %d files, %s\n", dirs, files, format.Bytes(bytesTotal))
}
