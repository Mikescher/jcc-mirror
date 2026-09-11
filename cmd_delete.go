package main

import (
	"context"
	"fmt"
	"os"
	"os/user"

	"blackforestbytes.com/jcc-mirror/engine"
	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/store"
)

// cmdDelete is the shrinking half of a mirror pair, and the only command in
// jcc-mirror that takes anything away. It runs on its own here because a sync
// runs it too: after the queue is empty, never during (DESIGN.md §2.5).
//
// Nothing is unlinked. A deleted file moves into <destdir>/.jccmirror/trash/<day>
// and stays there for the retention, so the answer to a diff that turned out to
// be wrong is a move back rather than a re-transfer of 30 TB.
func cmdDelete(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("delete")
	ref := fs.String("pair", "", "the pair to shrink, by name or id")
	approve := fs.Bool("approve", false, "allow the deletion this pair is holding, then carry it out")
	reject := fs.Bool("reject", false, "refuse the deletion this pair is holding and leave the files alone")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *approve && *reject {
		return fmt.Errorf("-approve and -reject say opposite things")
	}

	logger := &logs.Logger{Verbose: cfg.verbose}
	eng, st, pair, closeFn, err := cfg.openEngine(ctx, logger, pickRef(*ref, first(fs.Args())))
	if err != nil {
		return err
	}
	defer closeFn()

	switch {
	case *reject:
		decided, err := st.DecideDeletion(ctx, pair.ID, store.ApprovalRejected, cliActor())
		if err != nil {
			return err
		}
		fmt.Printf("\nthe deletion of %s file(s) from %q was refused; nothing was touched\n",
			format.Comma(decided.Files), pair.Name)
		return nil

	case *approve:
		decided, err := st.DecideDeletion(ctx, pair.ID, store.ApprovalApproved, cliActor())
		if err != nil {
			return err
		}
		fmt.Printf("\napproved: up to %s file(s), %s, from the walk this was computed against\n",
			format.Comma(decided.Files), format.Bytes(decided.Bytes))
	}

	res, err := eng.Reap(ctx, pair)
	printReap(pair, res)
	return err
}

// printReap says what the deletion phase did, or - the more important case - what
// it refused to do and how to allow it.
func printReap(pair store.Pair, res engine.ReapResult) {
	if len(res.Pruned) > 0 {
		fmt.Printf("\nquarantine of %q emptied: %s freed from %d expired day(s)\n",
			pair.Name, format.Bytes(res.PrunedBytes), len(res.Pruned))
	}

	if b := res.Blocked; b != nil {
		fmt.Printf("\ndeletion of %q is on hold\n", pair.Name)
		fmt.Printf("  files          %s\n", format.Comma(b.Files))
		fmt.Printf("  bytes          %s\n", format.Bytes(b.Bytes))
		fmt.Printf("  reason         %s\n", b.Reason)
		fmt.Printf("\n=> nothing was deleted. `jcc-mirror plan -pair %s` lists what would go;\n", pair.Name)
		fmt.Printf("   `jcc-mirror delete -pair %s -approve` allows exactly this set, and\n", pair.Name)
		fmt.Printf("   `-reject` leaves it alone. A later scan makes the approval stale on purpose.\n")
		return
	}

	if res.Skipped != "" {
		fmt.Printf("\nnothing deleted from %q: %s\n", pair.Name, res.Skipped)
		return
	}

	fmt.Printf("\ndeletion from %q finished in %s\n", pair.Name, format.Duration(res.Duration))
	fmt.Printf("  quarantined    %s files, %s\n", format.Comma(int64(res.Deleted)), format.Bytes(res.Bytes))
	fmt.Printf("  already gone   %s files were no longer on disk\n", format.Comma(int64(res.Missing)))
	fmt.Printf("  failed         %s files could not be moved\n", format.Comma(int64(res.Failed)))

	if res.Deleted > 0 {
		fmt.Printf("\n=> nothing was unlinked: `jcc-mirror trash -pair %s` lists what is held and\n", pair.Name)
		fmt.Printf("   can put a file back until its retention runs out.\n")
	}
}

// cliActor labels the record a decision leaves. The dashboard writes the address
// it was pressed from; a shell has a user instead.
func cliActor() string {
	name := "cli"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return name + "@" + host
	}
	return name
}
