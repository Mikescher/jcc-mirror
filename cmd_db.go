package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"blackforestbytes.com/jcc-mirror/engine"
	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/store"
)

// cmdDB is the jCC pair's shared database: what each side holds, which locks are
// out, and the copies kept of the ones that were replaced (DESIGN.md §3, S5).
//
// It is a command of its own because the database is the one file jcc-mirror
// treats differently. A sync runs the same gate at the end of its own run; this
// is how to look at it, run it now, override a lock that will never clear, and
// put a copy back when a transfer went wrong.
func cmdDB(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("db")
	ref := fs.String("pair", "", "the jcc pair whose database to look at, by name or id")
	sync := fs.Bool("sync", false, "run the lock gate now: copy the publisher's database if neither side has it open")
	rollback := fs.Bool("rollback", false, "put a kept copy back in place of what is here")
	backup := fs.Int64("backup", 0, "which copy to roll back to, by id; the newest by default")
	force := fs.Bool("force", false, "carry on although a lock is held - for one that has gone stale and will never clear")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sync && *rollback {
		return fmt.Errorf("-sync and -rollback say opposite things")
	}

	logger := &logs.Logger{Verbose: cfg.verbose}

	// A rollback asks the publisher nothing, and the tunnel being down is one of
	// the reasons to want one. It gets an engine that cannot reach him at all.
	if *rollback {
		eng, st, closeFn, err := cfg.openLocalEngine(ctx, logger)
		if err != nil {
			return err
		}
		defer closeFn()

		pair, err := openPair(ctx, st, pickRef(*ref, first(fs.Args())))
		if err != nil {
			return err
		}
		res, err := eng.RollbackDatabase(ctx, pair, *backup, *force)
		if err != nil {
			return err
		}
		return printRollback(res)
	}

	eng, st, closeFn, err := cfg.openEngine(ctx, logger)
	if err != nil {
		return err
	}
	defer closeFn()

	pair, err := openPair(ctx, st, pickRef(*ref, first(fs.Args())))
	if err != nil {
		return err
	}

	if *sync {
		res, err := eng.SyncDatabase(ctx, pair, *force)
		if err != nil {
			return err
		}
		printDatabaseRun(pair, res)
		return nil
	}

	status, err := eng.DatabaseStatus(ctx, pair)
	if err != nil {
		return err
	}
	return printDatabaseStatus(status)
}

// printDatabaseStatus is the whole picture in one screen: both copies, both
// locks, and what is kept here.
func printDatabaseStatus(s engine.DBStatus) error {
	fmt.Printf("\ndatabase of %q: %s\n", s.PairName, s.Path)
	fmt.Printf("  publisher      %s\n", dbSide(s.Remote))
	fmt.Printf("  here           %s\n", dbSide(s.Local))

	for _, l := range s.Locks {
		fmt.Printf("  %-14s %s\n", lockLabel(l), lockText(l))
	}

	if len(s.Backups) > 0 {
		fmt.Printf("\nkept copies, newest first\n\n")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprint(w, "  id\ttaken\tsize\tdated\twhy\n")
		for _, b := range s.Backups {
			fmt.Fprintf(w, "  %d\t%s\t%s\t%s\t%s\n", b.ID,
				b.TS.Local().Format("2006-01-02 15:04:05"), format.Bytes(b.Size),
				b.MTime.Local().Format("2006-01-02 15:04:05"), b.Reason)
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}

	fmt.Println()
	switch {
	case s.Remote == nil:
		fmt.Printf("=> the publisher has no database at that path. Check the pair's remote directory\n   and the jcc.db_dir and jcc.db_name settings.\n")
	case s.InSync:
		fmt.Printf("=> the two agree; nothing would be copied.\n")
	case len(s.Held()) > 0:
		fmt.Printf("=> the database differs, and it will not be copied while a lock is held. That is\n   the rule, not a fault: no lock means a clean shutdown, which is what makes a\n   plain byte copy of a database consistent.\n")
		for _, l := range s.Held() {
			if l.Stale {
				fmt.Printf("=> that lock is stale. jClipCorn deletes its own on a clean shutdown, so nothing\n   will ever clear this one: `jcc-mirror db -pair %s -sync -force` copies anyway.\n", s.PairName)
			}
		}
	default:
		fmt.Printf("=> the database differs and neither side has it open: `jcc-mirror db -pair %s -sync`\n   copies it now, and a sync of the pair does the same at the end of its run.\n", s.PairName)
	}
	return nil
}

func printDatabaseRun(pair store.Pair, res engine.DBResult) {
	if res.Skipped != "" {
		fmt.Printf("\nnothing was copied for %q: %s\n", pair.Name, res.Skipped)
		return
	}
	if res.Deferred != "" {
		fmt.Printf("\nthe database of %q was left alone: %s\n", pair.Name, res.Deferred)
		fmt.Printf("\n=> nothing here was touched. The next cycle probes again; `-force` copies\n   anyway, which is meant for a lock that has gone stale.\n")
		return
	}

	fmt.Printf("\n%s of %q replaced in %s\n", res.Path, pair.Name, format.Duration(res.Duration))
	fmt.Printf("  copied         %s, dated %s\n", format.Bytes(res.Bytes), res.MTime.Local().Format("2006-01-02 15:04:05"))
	if res.Backup != nil {
		fmt.Printf("  kept           %s (id %d)\n", res.Backup.Path, res.Backup.ID)
	} else {
		fmt.Printf("  kept           nothing: there was no database here to replace\n")
	}
	if res.Forced {
		fmt.Printf("  forced         a held lock was overridden\n")
	}
	if res.Backup != nil {
		fmt.Printf("\n=> `jcc-mirror db -pair %s -rollback` puts that copy back if this one turns out\n   to be bad.\n", pair.Name)
	}
}

func printRollback(res engine.RollbackResult) error {
	fmt.Printf("\n%s of %q rolled back\n", res.Path, res.PairName)
	fmt.Printf("  restored       the copy of %s, %s\n",
		res.Backup.TS.Local().Format("2006-01-02 15:04:05"), format.Bytes(res.Backup.Size))
	if res.Replaced != nil {
		fmt.Printf("  kept           what it displaced, as id %d\n", res.Replaced.ID)
	}
	fmt.Printf("\n=> the next sync compares this against the publisher's copy and will fetch his\n   again, which is what you want if the transfer was the problem. Disable the\n   pair first if it is not.\n")
	return nil
}

func dbSide(f *engine.DBFile) string {
	if f == nil {
		return "nothing there"
	}
	return fmt.Sprintf("%s, dated %s", format.Bytes(f.Size), f.MTime.Local().Format("2006-01-02 15:04:05"))
}

func lockLabel(l engine.LockState) string {
	if l.Side == engine.SidePublisher {
		return "lock there"
	}
	return "lock here"
}

func lockText(l engine.LockState) string {
	if !l.Held {
		return "none, so the database may be copied"
	}
	age := "of unknown age"
	if !l.MTime.IsZero() {
		age = "unchanged for " + format.Duration(l.Age.Round(time.Second))
	}
	if l.Stale {
		return "held, " + age + " - stale, and it will not clear on its own"
	}
	return "held, " + age
}
