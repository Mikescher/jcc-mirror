package main

import (
	"blackforestbytes.com/jcc-mirror/engine"
	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/logs"
	"context"
	"fmt"
)

// cmdAdopt is the USB bootstrap being recognised. The first ~30 TB comes across
// by hand, and without this the first sync would look at a full Synology and
// transfer every byte of it again down a domestic line.
//
// The match is on size alone. Mtimes are not compared and not trusted: a USB copy
// that did not preserve them would make all 30 TB look changed. What gets
// recorded as local truth is the publisher's timestamp, so the next diff agrees
// with the manifest (DESIGN.md §2.4).
func cmdAdopt(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("adopt")
	ref := fs.String("pair", "", "the pair to adopt, by name or id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := &logs.Logger{Verbose: cfg.verbose}
	eng, _, pair, closeFn, err := cfg.openEngine(ctx, logger, pickRef(*ref, first(fs.Args())))
	if err != nil {
		return err
	}
	defer closeFn()

	res, adoptErr := eng.Adopt(ctx, pair)
	if res.RemoteFiles > 0 {
		printAdopt(res, adoptErr != nil)
	}
	return adoptErr
}

func printAdopt(res engine.AdoptResult, stoppedEarly bool) {
	verb := "finished"
	if stoppedEarly {
		verb = "stopped early"
	}

	fmt.Printf("\nadopt of %q %s after %s\n", res.PairName, verb, format.Duration(res.Duration))
	fmt.Printf("  matched        %s files, %s - same size as the publisher's, recorded as mirrored\n",
		format.Comma(int64(res.Matched)), format.Bytes(res.MatchedBytes))
	fmt.Printf("  already known  %s files were local truth already and were left alone\n", format.Comma(int64(res.AlreadyKnown)))
	fmt.Printf("  size mismatch  %s files are here at a different size and will transfer\n", format.Comma(int64(res.SizeMismatch)))
	fmt.Printf("  missing        %s files are on the publisher and not here, and will transfer\n", format.Comma(int64(res.Missing)))
	fmt.Printf("  extra          %s files, %s are here and not on the publisher; nothing is done about them\n",
		format.Comma(int64(res.Extra)), format.Bytes(res.ExtraBytes))
	fmt.Printf("  excluded       %s local files the pair's globs do not cover\n", format.Comma(int64(res.Excluded)))
	fmt.Printf("  manifest       %s files on the publisher for comparison\n", format.Comma(res.RemoteFiles))

	fmt.Printf("\n=> matched on size alone, which is the point: an mtime a USB copy did not\n   preserve would otherwise make every one of those files look changed.\n")
	fmt.Printf("=> check it with `jcc-mirror plan -pair %s`. If that still wants tens of\n   thousands of files, the adoption did not take - better found out now than\n   with the transfer already running.\n", res.PairName)
}
