package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"blackforestbytes.com/jcc-mirror/engine"
	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/store"
)

// cmdPairs configures what is mirrored. A pair is one directory of one of the
// publisher's remotes mapped onto one directory here (DESIGN.md §6).
//
// Nothing here touches a file on disk. Adding a pair records an intention;
// removing one forgets what was recorded about it, and leaves everything that was
// ever mirrored exactly where it is.
func cmdPairs(ctx context.Context, args []string) error {
	verb := "list"
	if word, rest := takeWord(args); word != "" {
		verb, args = word, rest
	}

	switch verb {
	case "list":
		return pairsList(ctx, args)
	case "add":
		return pairsAdd(ctx, args)
	case "set":
		return pairsSet(ctx, args)
	case "rm":
		return pairsRemove(ctx, args)
	default:
		return fmt.Errorf("unknown verb %q: usage is `pairs [list|add|set|rm] [flags]`", verb)
	}
}

func pairsList(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("pairs list")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := cfg.openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	pairs, err := st.Pairs(ctx)
	if err != nil {
		return err
	}
	if len(pairs) == 0 {
		fmt.Printf("no pairs configured; `jcc-mirror pairs add -h` says how to make one\n")
		return nil
	}

	remotes, err := st.Remotes(ctx)
	if err != nil {
		return err
	}
	remoteNames := map[int64]string{}
	for _, r := range remotes {
		remoteNames[r.ID] = r.Name
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprint(w, "  id\tname\ttype\tmode\tprio\ton\tfrom\tremote\tlocal\n")
	for _, p := range pairs {
		fmt.Fprintf(w, "  %d\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			p.ID, p.Name, p.Type, p.Mode, p.Priority, yesNo(p.Enabled),
			orDash(remoteNames[p.RemoteID]), orDash(p.RemotePath), p.LocalPath)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	for _, p := range pairs {
		if err := printPairState(ctx, st, p); err != nil {
			return err
		}
	}
	return nil
}

// printPairState is the answer to "how far behind is this pair": what the last
// walk found on the publisher, what is recorded here, and what is still queued.
func printPairState(ctx context.Context, st *store.Store, p store.Pair) error {
	remoteFiles, remoteBytes, err := st.ManifestStats(ctx, p.ID)
	if err != nil {
		return err
	}
	localFiles, localBytes, err := st.FileStats(ctx, p.ID)
	if err != nil {
		return err
	}
	queue, err := st.Queue(ctx, p.ID)
	if err != nil {
		return err
	}
	scan, scanned, err := st.LastCompletedScan(ctx, p.ID)
	if err != nil {
		return err
	}

	fmt.Printf("\n%s (#%d)\n", p.Name, p.ID)
	if p.RemoteID == 0 {
		fmt.Printf("  remote         none - nothing runs until `jcc-mirror pairs set %s -from <remote>`\n", p.Name)
	} else {
		r, err := st.RemoteByID(ctx, p.RemoteID)
		if err != nil {
			return err
		}
		fmt.Printf("  remote         %q, %s\n", r.Name, remoteTarget(r))
	}
	if len(p.Includes) > 0 {
		fmt.Printf("  includes       %s\n", strings.Join(p.Includes, ", "))
	}
	if len(p.Excludes) > 0 {
		fmt.Printf("  excludes       %s\n", strings.Join(p.Excludes, ", "))
	}
	if p.Owner != "" || p.FileMode != "" || p.DirMode != "" {
		fmt.Printf("  new entries    owner %s, files %s, directories %s\n", orDash(p.Owner), orDash(p.FileMode), orDash(p.DirMode))
	}
	if p.DeleteGuard > 0 {
		fmt.Printf("  delete guard   %s files before a deletion waits for approval\n", format.Comma(int64(p.DeleteGuard)))
	}

	if scanned && scan.FinishedAt != nil {
		fmt.Printf("  last scan      %s\n", scan.FinishedAt.Format("2006-01-02 15:04:05"))
	} else {
		fmt.Printf("  last scan      never - until one finishes there is no manifest to diff against\n")
	}
	fmt.Printf("  publisher      %s files, %s\n", format.Comma(remoteFiles), format.Bytes(remoteBytes))
	fmt.Printf("  here           %s files, %s\n", format.Comma(localFiles), format.Bytes(localBytes))
	fmt.Printf("  queue          %s\n", queueLine(queue))
	if p.Type == store.PairJCC {
		fmt.Printf("  database       gated: copied only while neither side has it open, and never\n                 through the queue; `jcc-mirror db -pair %s` says where it stands\n", p.Name)
	}
	return nil
}

func pairsAdd(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("pairs add")
	p, enabled := pairFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	p.Enabled = *enabled

	st, err := cfg.openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	r, err := remoteForNewPair(ctx, st, cfg.from, given["from"])
	if err != nil {
		return err
	}
	p.RemoteID = r.ID

	if err := st.CreatePair(ctx, p); err != nil {
		return err
	}
	recordPairChange(ctx, st, *p, "added")

	fmt.Printf("added pair %q (#%d)\n", p.Name, p.ID)
	if p.RemoteID != 0 {
		fmt.Printf("  remote         %q, %s\n", r.Name, remoteTarget(r))
	}
	fmt.Printf("  publisher      %s\n", orDash(p.RemotePath))
	fmt.Printf("  here           %s\n", p.LocalPath)
	if p.RemoteID == 0 {
		fmt.Printf("\n=> the pair reads from no remote, so nothing can run yet: add one with\n   `jcc-mirror remotes add`, then `jcc-mirror pairs set %s -from <remote>`\n", p.Name)
		return nil
	}
	fmt.Printf("\n=> nothing is mirrored yet: `scan -pair %s`, then `plan`, then `sync`\n", p.Name)
	return nil
}

// remoteForNewPair is the remote a new pair reads from. Without -from, one
// stored remote is the only answer; with several, a guess would mirror the wrong
// share, so the choice is asked for.
func remoteForNewPair(ctx context.Context, st *store.Store, from string, given bool) (store.Remote, error) {
	if given {
		return pairRemote(ctx, st, from)
	}

	remotes, err := st.Remotes(ctx)
	if err != nil {
		return store.Remote{}, err
	}
	switch len(remotes) {
	case 0:
		return store.Remote{}, nil
	case 1:
		return remotes[0], nil
	}

	names := make([]string, len(remotes))
	for i, r := range remotes {
		names[i] = r.Name
	}
	return store.Remote{}, fmt.Errorf("%d remotes are stored (%s): say which one the pair reads from with -from",
		len(remotes), strings.Join(names, ", "))
}

// pairRemote resolves -from as the remote a pair reads from, where an empty
// value given on purpose means none.
func pairRemote(ctx context.Context, st *store.Store, ref string) (store.Remote, error) {
	if strings.TrimSpace(ref) == "" {
		return store.Remote{}, nil
	}
	return st.FindRemote(ctx, ref)
}

func pairsSet(ctx context.Context, args []string) error {
	word, args := takeWord(args)

	fs, cfg := newFlagSet("pairs set")
	edit, enabled := pairFlags(fs)
	ref := fs.String("pair", "", "the pair to change, by name or id; may also be given as the first argument")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Only the flags that were actually typed are written back: without this an
	// unmentioned -name would blank the name, and every -enabled left off would
	// switch the pair back on.
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	var changed []string
	for _, name := range pairFields {
		if given[name] {
			changed = append(changed, name)
		}
	}
	if len(changed) == 0 {
		return fmt.Errorf("nothing to change: give at least one of -%s", strings.Join(pairFields, ", -"))
	}

	st, err := cfg.openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	p, err := openPair(ctx, st, pickRef(word, *ref, first(fs.Args())))
	if err != nil {
		return err
	}

	for _, name := range changed {
		switch name {
		case "name":
			p.Name = edit.Name
		case "type":
			p.Type = edit.Type
		case "from":
			r, err := pairRemote(ctx, st, cfg.from)
			if err != nil {
				return err
			}
			p.RemoteID = r.ID
		case "remote":
			p.RemotePath = edit.RemotePath
		case "local":
			p.LocalPath = edit.LocalPath
		case "mode":
			p.Mode = edit.Mode
		case "include":
			// The globs given replace the pair's list rather than adding to it; there
			// would otherwise be no way to take one back off.
			p.Includes = edit.Includes
		case "exclude":
			p.Excludes = edit.Excludes
		case "priority":
			p.Priority = edit.Priority
		case "guard":
			p.DeleteGuard = edit.DeleteGuard
		case "owner":
			p.Owner = edit.Owner
		case "file-mode":
			p.FileMode = edit.FileMode
		case "dir-mode":
			p.DirMode = edit.DirMode
		case "enabled":
			p.Enabled = *enabled
		}
	}

	if err := st.UpdatePair(ctx, &p); err != nil {
		return err
	}
	recordPairChange(ctx, st, p, "changed")

	fmt.Printf("changed %s on pair %q (#%d)\n", strings.Join(changed, ", "), p.Name, p.ID)

	// A pair whose paths or globs moved describes a different set of files, and the
	// manifest still describes the old one.
	if given["from"] || given["remote"] || given["local"] || given["include"] || given["exclude"] {
		fmt.Printf("\n=> the pair covers something else now; scan it again before the next sync\n")
	}
	return printPairState(ctx, st, p)
}

func pairsRemove(ctx context.Context, args []string) error {
	word, args := takeWord(args)

	fs, cfg := newFlagSet("pairs rm")
	ref := fs.String("pair", "", "the pair to remove, by name or id; may also be given as the first argument")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := cfg.openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	p, err := openPair(ctx, st, pickRef(word, *ref, first(fs.Args())))
	if err != nil {
		return err
	}

	remoteFiles, _, err := st.ManifestStats(ctx, p.ID)
	if err != nil {
		return err
	}
	localFiles, localBytes, err := st.FileStats(ctx, p.ID)
	if err != nil {
		return err
	}
	queue, err := st.Queue(ctx, p.ID)
	if err != nil {
		return err
	}

	// Before the delete: the event's pair_id is a foreign key that is set to null
	// rather than cascaded, so the row survives the pair it describes.
	recordPairChange(ctx, st, p, "removed")

	if err := st.DeletePair(ctx, p.ID); err != nil {
		return err
	}

	var queued int64
	for _, n := range queue.Counts {
		queued += n
	}
	fmt.Printf("removed pair %q (#%d)\n", p.Name, p.ID)
	fmt.Printf("  manifest       %s rows dropped\n", format.Comma(remoteFiles))
	fmt.Printf("  local truth    %s rows dropped (%s worth of files)\n", format.Comma(localFiles), format.Bytes(localBytes))
	fmt.Printf("  queue          %s jobs dropped\n", format.Comma(queued))
	fmt.Printf("\n=> only the records are gone. Nothing under %s was touched, including the\n   partials in %s\n", p.LocalPath, filepath.Join(p.LocalPath, engine.PartDir))
	return nil
}

// pairFields are the flags that describe a pair, in the order they are reported.
var pairFields = []string{"name", "type", "from", "remote", "local", "mode", "include", "exclude", "priority", "guard", "owner", "file-mode", "dir-mode", "enabled"}

// pairFlags registers those fields on a flag set and returns the pair they fill,
// all but -from, which is one of the flags every command shares. The defaults are
// what `add` wants; `set` reads fs.Visit instead and ignores every value that was
// not typed.
func pairFlags(fs *flag.FlagSet) (*store.Pair, *bool) {
	p := &store.Pair{}

	fs.StringVar(&p.Name, "name", "", "what the pair is called; every other command refers to it by this or by its id")
	fs.StringVar(&p.Type, "type", store.PairRaw, "\"raw\", or \"jcc\" for the ClipCornDB directory: hard exclusions and a lock gate on the database")
	fs.StringVar(&p.RemotePath, "remote", "", "directory on the pair's remote, relative to that remote's root; empty means the root itself")
	fs.StringVar(&p.LocalPath, "local", "", "absolute directory here that the pair mirrors into")
	fs.StringVar(&p.Mode, "mode", store.ModeAdditive, "\"additive\" or \"mirror\"; a mirror pair quarantines what the publisher drops, behind the guards")
	fs.IntVar(&p.Priority, "priority", 100, "lower runs first")
	fs.IntVar(&p.DeleteGuard, "guard", 100, "hold a deletion of more than this many files until it is approved; 0 removes the count limit, leaving the percentage threshold")
	fs.StringVar(&p.Owner, "owner", "", "numeric uid:gid given to every file and directory the mirror creates, e.g. 1026:100; needs root; empty keeps the process's own")
	fs.StringVar(&p.FileMode, "file-mode", "", "octal mode set on every file the mirror creates, e.g. 0000 for a Synology ACL share; empty is 0644 minus the umask")
	fs.StringVar(&p.DirMode, "dir-mode", "", "octal mode set on every directory the mirror creates; empty is 0755 minus the umask")
	enabled := fs.Bool("enabled", true, "whether the pair is mirrored at all")

	fs.Var(&listFlag{dst: &p.Includes}, "include", "glob a path must match to be mirrored; repeat the flag, or separate several with commas")
	fs.Var(&listFlag{dst: &p.Excludes}, "exclude", "glob that drops a path, and for a directory its whole subtree; repeat the flag, or separate several with commas")

	return p, enabled
}

// recordPairChange puts the change in the same event log the dashboard's config
// changes land in. Failure is logged rather than returned: the pair is already
// written, and telemetry must not turn a successful change into a failed command.
func recordPairChange(ctx context.Context, st *store.Store, p store.Pair, what string) {
	id := p.ID
	err := st.AppendEvent(ctx, &store.Event{
		Level:   store.LevelInfo,
		Kind:    store.KindPairChanged,
		PairID:  &id,
		Message: fmt.Sprintf("pair %q %s from the command line", p.Name, what),
		Data: map[string]any{
			"action": what, "id": p.ID, "name": p.Name, "type": p.Type, "mode": p.Mode,
			"remoteId": p.RemoteID, "remotePath": p.RemotePath, "localPath": p.LocalPath,
			"owner": p.Owner, "fileMode": p.FileMode, "dirMode": p.DirMode, "enabled": p.Enabled,
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: the change was made but not recorded in the event log: %v\n", err)
	}
}

func first(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}
