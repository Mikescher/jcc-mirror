package main

import (
	"context"
	"fmt"

	"blackforestbytes.com/jcc-mirror/engine"
	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/store"
)

// cmdScan builds the manifest. The publisher runs nothing of ours and offers no
// file list, so the only way to know what he has is to walk the share one
// PROPFIND per directory - and that walk is what every later command reads
// (DESIGN.md §2.3).
//
// It is also the measurement the schedule rests on: a scan of the real collection
// runs for minutes, and how many it runs for is what decides how often one can be
// afforded.
func cmdScan(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("scan")
	ref := fs.String("pair", "", "the pair to walk, by name or id")
	resume := fs.Bool("resume", true, "continue an interrupted walk where it stopped; -resume=false starts over")
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

	res, scanErr := eng.Scan(ctx, pair, *resume)
	if res.Scan.ID != 0 {
		printScan(pair, res)
	}
	return scanErr
}

func printScan(pair store.Pair, res engine.ScanResult) {
	verb := "finished"
	if res.Scan.State == store.ScanInterrupted {
		verb = "stopped early"
	}

	fmt.Printf("\nscan of %q %s in %s\n", pair.Name, verb, format.Duration(res.Duration))
	fmt.Printf("  directories    %s\n", format.Comma(res.Scan.Dirs))
	fmt.Printf("  files          %s\n", format.Comma(res.Scan.Files))
	fmt.Printf("  bytes          %s\n", format.Bytes(res.Scan.Bytes))
	fmt.Printf("  swept          %s rows the publisher no longer has\n", format.Comma(res.Swept))
	fmt.Printf("  state          %s\n", res.Scan.State)
	if res.Scan.Error != "" {
		fmt.Printf("  reason         %s\n", res.Scan.Error)
	}

	// The counters above belong to the whole walk, the duration only to this run,
	// so a resumed scan does not say what a full one costs.
	switch {
	case res.Scan.State == store.ScanInterrupted:
		fmt.Printf("\n=> the manifest is incomplete and nothing was swept; scan again to carry on\n   from directory %s\n", format.Comma(res.Scan.Dirs+1))
	case res.Resumed:
		fmt.Printf("\n=> this run only finished a walk an earlier one started, so %s is not what a\n   full scan costs\n", format.Duration(res.Duration))
	default:
		fmt.Printf("\n=> one full scan costs %s; that is the floor for the scan schedule (DESIGN.md §6, §10.4)\n", format.Duration(res.Duration))
	}
}
