package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"text/tabwriter"

	"blackforestbytes.com/jcc-mirror/engine"
	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/store"
)

// cmdPlan is the diff, printed and not acted on. Every plan can be computed and
// displayed before anything executes (DESIGN.md §6), and this is where a diff
// that has gone wrong shows up cheaply: a plan that wants to move the whole
// collection again means the mtime tolerance, the NFC normalization or the
// chtimes after download is broken, not that the publisher replaced 30 TB.
func cmdPlan(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("plan")
	ref := fs.String("pair", "", "the pair to diff, by name or id")
	limit := fs.Int("limit", 25, "how many entries to list; the counts above them are always complete")
	all := fs.Bool("all", false, "list every entry, however many there are")
	if err := fs.Parse(args); err != nil {
		return err
	}

	sample := *limit
	if *all {
		sample = math.MaxInt
	}
	if sample < 0 {
		return fmt.Errorf("-limit cannot be negative")
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

	plan, err := eng.Plan(ctx, pair, sample)
	if err != nil {
		return err
	}
	return printPlan(plan)
}

func printPlan(p engine.Plan) error {
	fmt.Printf("\nplan for %q, against the walk of %s\n", p.PairName, p.ScannedAt.Format("2006-01-02 15:04:05"))
	fmt.Printf("  add            %s files, %s\n", format.Comma(int64(p.Add)), format.Bytes(p.AddBytes))
	fmt.Printf("  replace        %s files, %s\n", format.Comma(int64(p.Replace)), format.Bytes(p.ReplaceBytes))
	fmt.Printf("  vanished       %s files, %s (%s)\n", format.Comma(int64(p.Vanished)), format.Bytes(p.VanishedBytes), p.Mode)
	if p.Excluded > 0 {
		fmt.Printf("  excluded       %s differences dropped by the pair's globs\n", format.Comma(int64(p.Excluded)))
	}
	fmt.Printf("  to transfer    %s files, %s\n", format.Comma(int64(p.Transfers())), format.Bytes(p.TransferBytes()))
	fmt.Printf("\n  publisher      %s files, %s\n", format.Comma(p.RemoteFiles), format.Bytes(p.RemoteBytes))
	fmt.Printf("  here           %s files, %s\n", format.Comma(p.LocalFiles), format.Bytes(p.LocalBytes))
	if p.Free > 0 {
		fmt.Printf("  free here      %s, keeping %s in reserve\n", format.Bytes(p.Free), format.Bytes(p.Reserve))
	}

	if len(p.Entries) > 0 {
		fmt.Println()
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprint(w, "  op\tsize\tpath\n")
		for _, e := range p.Entries {
			fmt.Fprintf(w, "  %s\t%s\t%s\n", e.Op, format.Bytes(entrySize(e)), e.Path)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if p.Truncated {
			fmt.Printf("  ... and more; -limit lists further, -all lists them all\n")
		}
	}

	fmt.Printf("\n=> nothing was changed here and nothing at all was asked of the publisher.\n")
	if p.Shortfall > 0 {
		// The preflight refuses this plan, so saying so here is what makes a dry
		// run worth doing (DESIGN.md §2.6).
		fmt.Printf("=> this does not fit: %s short. A sync would be refused before it started;\n   narrow the pair with excludes, or lower the reserve.\n", format.Bytes(p.Shortfall))
	}
	if p.Vanished > 0 {
		// The count is the point of it: a diff that has gone wrong shows up as a
		// vanished count in the thousands long before anything acts on one.
		switch {
		case p.Mode != store.ModeMirror:
			fmt.Printf("=> the %s vanished file(s) are counted and will not be deleted: this pair is\n   additive, so the tree here only ever grows.\n", format.Comma(int64(p.Vanished)))
		case p.Guard != "" && !p.Approved:
			fmt.Printf("=> the %s vanished file(s) are on hold: %s.\n   `jcc-mirror delete -pair %s -approve` allows exactly this set.\n",
				format.Comma(int64(p.Vanished)), p.Guard, p.PairName)
		case p.Guard != "":
			fmt.Printf("=> the %s vanished file(s) have been approved and go to the quarantine on the\n   next sync.\n", format.Comma(int64(p.Vanished)))
		default:
			fmt.Printf("=> the %s vanished file(s) move to .jccmirror/trash on the next sync, once the\n   additions have landed, and are removed for good when the retention runs out.\n", format.Comma(int64(p.Vanished)))
		}
	}
	if p.Database != nil {
		// The one file of a plan that is not a job. Saying so here is what keeps a
		// queue that is one shorter than the plan from looking like a bug.
		fmt.Printf("=> %s is the jCC database: it is counted above but never queued. A sync copies\n   it at the end of its run, between two lock probes, and `jcc-mirror db -pair %s`\n   says whether either side has it open right now.\n", p.Database.Path, p.PairName)
	}
	if p.Transfers() > 0 && p.Shortfall == 0 {
		fmt.Printf("=> `jcc-mirror sync -pair %s` queues and transfers this.\n", p.PairName)
	}
	return nil
}

// entrySize is what the entry is worth in bytes: for a vanished file that is what
// we hold, since the publisher's copy is the one that is gone.
func entrySize(e engine.PlanEntry) int64 {
	if e.Op == store.OpDelete {
		return e.SizeBefore
	}
	return e.Size
}
