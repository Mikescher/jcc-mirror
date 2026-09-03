package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"blackforestbytes.com/jcc-mirror/remote"
	"blackforestbytes.com/jcc-mirror/webdav"
)

// cmdPropfind is M0 step 3: one PROPFIND against the real share. It is the first
// thing that touches DSM, and -raw exists because the two things most likely to
// be wrong - href shape and namespace prefixes - are only visible in the XML.
func cmdPropfind(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("propfind")
	dir := fs.String("path", "", "remote directory, relative to -url")
	depth := fs.String("depth", webdav.Depth1, `Depth header: "0", "1" or "infinity"`)
	raw := fs.Bool("raw", false, "also print the raw XML response")
	limit := fs.Int("limit", 200, "print at most this many entries, 0 for all")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := &Logger{Verbose: cfg.verbose}
	client, _, closeFn, err := cfg.openBoth(logger)
	if err != nil {
		return err
	}
	defer closeFn()

	// The raw body is captured rather than streamed so it can be printed after a
	// decode failure too, which is when it is worth the most.
	var captured bytes.Buffer
	var sink io.Writer
	if *raw {
		sink = &captured
	}

	start := time.Now()
	entries, err := client.Propfind(ctx, *dir, *depth, sink)
	elapsed := time.Since(start)

	if *raw && captured.Len() > 0 {
		fmt.Fprintln(os.Stdout, captured.String())
	}
	if err != nil {
		return err
	}

	printEntries(entries, *dir, *limit)
	logger.Infof("propfind depth=%s %q: %d entries in %s", *depth, *dir, len(entries), humanDuration(elapsed))
	if n := client.NonNFCNames(); n > 0 {
		logger.Infof("note: %d name(s) were not NFC - the diff must normalize (DESIGN.md §2.3)", n)
	}
	return nil
}

// printEntries writes the listing as a fixed-width table. self is the directory
// that was listed; its own entry is marked rather than hidden, because "did the
// server include the collection itself" is a thing worth seeing once.
func printEntries(entries []remote.Entry, self string, limit int) {
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

		kind, size := "file", humanBytes(e.Size)
		if e.IsDir {
			kind, size = "dir", "-"
		}
		name := e.Path
		if name == "" || name == self {
			name += "   <- the collection itself"
		}
		fmt.Printf("%-4s  %12s  %s  %s\n", kind, size, e.MTime.Format(time.RFC3339), name)
	}

	if limit > 0 && len(entries) > limit {
		fmt.Printf("... %d more (raise -limit)\n", len(entries)-limit)
	}
	fmt.Printf("\n%d dirs, %d files, %s\n", dirs, files, humanBytes(bytesTotal))
}
