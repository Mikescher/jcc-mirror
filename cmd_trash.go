package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"blackforestbytes.com/jcc-mirror/engine"
	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/store"
)

// cmdTrash is the other side of the quarantine: what is held, what can be put
// back, and what has run out of retention. It reads the store only for the pair
// and its retention - the quarantine itself is a directory, deliberately, so that
// a copy of it can be rescued with nothing but a file manager (DESIGN.md §2.5).
func cmdTrash(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("trash")
	ref := fs.String("pair", "", "the pair whose quarantine to look at, by name or id")
	prune := fs.Bool("prune", false, "remove for good what the retention has run out on")
	restore := fs.String("restore", "", "move one file back where it was, by its path inside the pair")
	day := fs.String("day", "", "which day's quarantine to restore from, YYYY-MM-DD; the newest by default")
	if err := fs.Parse(args); err != nil {
		return err
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

	values, err := st.Config(ctx)
	if err != nil {
		return err
	}
	retention := values.Duration(store.KeyDeleteRetention)

	if *restore != "" {
		back, err := engine.RestoreTrash(pair, *day, *restore)
		if err != nil {
			return err
		}
		fmt.Printf("\n%s restored to %s (%s)\n", back.Path, pair.LocalPath, format.Bytes(back.Size))
		fmt.Printf("\n=> it is yours now: no row of local truth was written, so the next mirror run\n")
		fmt.Printf("   leaves it alone rather than quarantining it again. Exclude its path from the\n")
		fmt.Printf("   pair if the publisher might list it once more.\n")
		return nil
	}

	if *prune {
		removed, err := engine.PruneTrash(pair, retention, time.Now())
		if err != nil {
			return err
		}
		var bytes int64
		for _, d := range removed {
			bytes += d.Bytes
		}
		fmt.Printf("\n%d day(s) of %q emptied for good, %s freed\n", len(removed), pair.Name, format.Bytes(bytes))
	}

	days, err := engine.Trash(pair, retention)
	if err != nil {
		return err
	}
	if len(days) == 0 {
		fmt.Printf("\nthe quarantine of %q is empty\n", pair.Name)
		return nil
	}

	var (
		files int
		bytes int64
		now   = time.Now()
	)
	fmt.Printf("\nquarantine of %q, kept for %s\n\n", pair.Name, format.Duration(retention))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprint(w, "  day\tfiles\tsize\tgoes\n")
	for _, d := range days {
		files += d.Files
		bytes += d.Bytes
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", d.Day, format.Comma(int64(d.Files)), format.Bytes(d.Bytes), expiryText(d, now))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n  total          %s files, %s\n", format.Comma(int64(files)), format.Bytes(bytes))
	fmt.Printf("\n=> `jcc-mirror trash -pair %s -restore <path>` puts one back;\n", pair.Name)
	fmt.Printf("   `-prune` empties what has expired. A sync does the same sweep on its own.\n")
	return nil
}

func expiryText(d engine.TrashDay, now time.Time) string {
	switch {
	case d.Expires.IsZero():
		return "kept"
	case d.Expired(now):
		return "expired"
	default:
		return "in " + format.Duration(d.Expires.Sub(now))
	}
}
