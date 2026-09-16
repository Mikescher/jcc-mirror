package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"blackforestbytes.com/jcc-mirror/store"
)

// cmdRemotes configures where the pairs read from. A remote is one SMB share on
// the publisher's side, rooted at a directory inside it, with the account that
// reads it; every pair names the remote it reads from (DESIGN.md §6).
//
// The SMB flags are the ones every command shares, so -host and the rest mean
// the same here as on `ls` - here they are written down rather than tried.
func cmdRemotes(ctx context.Context, args []string) error {
	verb := "list"
	if word, rest := takeWord(args); word != "" {
		verb, args = word, rest
	}

	switch verb {
	case "list":
		return remotesList(ctx, args)
	case "add":
		return remotesAdd(ctx, args)
	case "set":
		return remotesSet(ctx, args)
	case "rm":
		return remotesRemove(ctx, args)
	default:
		return fmt.Errorf("unknown verb %q: usage is `remotes [list|add|set|rm] [flags]`", verb)
	}
}

func remotesList(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("remotes list")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := cfg.openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	remotes, err := st.Remotes(ctx)
	if err != nil {
		return err
	}
	if len(remotes) == 0 {
		fmt.Printf("no remotes configured; `jcc-mirror remotes add -h` says how to make one\n")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprint(w, "  id\tname\ttarget\tuser\tpassword\tpairs\n")
	for _, r := range remotes {
		pairs, err := st.PairNamesForRemote(ctx, r.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  %d\t%s\t%s\t%s\t%s\t%s\n",
			r.ID, r.Name, remoteTarget(r), remoteAccount(r), yesNo(r.PasswordSet), orDash(strings.Join(pairs, ", ")))
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Printf("\n=> %q is the first: the diagnostics read it when no -from says otherwise\n", remotes[0].Name)
	return nil
}

func remotesAdd(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("remotes add")
	name := fs.String("name", "", "what the remote is called; pairs and -from refer to it by this or by its id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := cfg.openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	r := &store.Remote{
		Name:     *name,
		Host:     cfg.smbHost,
		Share:    cfg.smbShare,
		Path:     cfg.smbPath,
		User:     cfg.smbUser,
		Password: cfg.smbPass,
		Domain:   cfg.smbDomain,
	}
	if err := st.CreateRemote(ctx, r); err != nil {
		return err
	}
	recordRemoteChange(ctx, st, *r, "added")

	fmt.Printf("added remote %q (#%d)\n", r.Name, r.ID)
	fmt.Printf("  target         %s\n", remoteTarget(*r))
	fmt.Printf("  account        %s, %s\n", remoteAccount(*r), passwordText(*r))
	fmt.Printf("\n=> `jcc-mirror ls -data %s -from %s` lists its root through the tunnel;\n   `jcc-mirror pairs set <pair> -from %s` points a pair at it\n", cfg.dataDir, r.Name, r.Name)
	return nil
}

// remoteFields are the flags that describe a remote, in the order they are
// reported.
var remoteFields = []string{"name", "host", "share", "share-path", "user", "pass", "domain"}

func remotesSet(ctx context.Context, args []string) error {
	word, args := takeWord(args)

	fs, cfg := newFlagSet("remotes set")
	name := fs.String("name", "", "the remote's new name")
	ref := fs.String("remote", "", "the remote to change, by name or id; may also be given as the first argument")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Only the flags that were actually typed are written back, as for a pair;
	// that is also what lets an explicit -pass "" clear the stored password.
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	var changed []string
	for _, f := range remoteFields {
		if given[f] {
			changed = append(changed, f)
		}
	}
	if len(changed) == 0 {
		return fmt.Errorf("nothing to change: give at least one of -%s", strings.Join(remoteFields, ", -"))
	}

	st, err := cfg.openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	r, err := openRemote(ctx, st, pickRef(word, *ref, first(fs.Args())))
	if err != nil {
		return err
	}

	for _, f := range changed {
		switch f {
		case "name":
			r.Name = *name
		case "host":
			r.Host = cfg.smbHost
		case "share":
			r.Share = cfg.smbShare
		case "share-path":
			r.Path = cfg.smbPath
		case "user":
			r.User = cfg.smbUser
		case "pass":
			r.Password = cfg.smbPass
		case "domain":
			r.Domain = cfg.smbDomain
		}
	}

	if err := st.UpdateRemote(ctx, &r); err != nil {
		return err
	}
	recordRemoteChange(ctx, st, r, "changed")

	fmt.Printf("changed %s on remote %q (#%d)\n", strings.Join(changed, ", "), r.Name, r.ID)
	fmt.Printf("  target         %s\n", remoteTarget(r))
	fmt.Printf("  account        %s, %s\n", remoteAccount(r), passwordText(r))

	// The pairs' paths are relative to the remote's root, so moving the root
	// moves every pair on it, and their manifests describe the old place.
	if given["host"] || given["share"] || given["share-path"] {
		pairs, err := st.PairNamesForRemote(ctx, r.ID)
		if err != nil {
			return err
		}
		if len(pairs) > 0 {
			fmt.Printf("\n=> %s read from it and cover something else now; scan them again before\n   the next sync\n", strings.Join(pairs, ", "))
		}
	}
	return nil
}

func remotesRemove(ctx context.Context, args []string) error {
	word, args := takeWord(args)

	fs, cfg := newFlagSet("remotes rm")
	ref := fs.String("remote", "", "the remote to remove, by name or id; may also be given as the first argument")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := cfg.openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	r, err := openRemote(ctx, st, pickRef(word, *ref, first(fs.Args())))
	if err != nil {
		return err
	}
	if err := st.DeleteRemote(ctx, r.ID); err != nil {
		return err
	}
	recordRemoteChange(ctx, st, r, "removed")

	fmt.Printf("removed remote %q (#%d)\n", r.Name, r.ID)
	return nil
}

// openRemote resolves the remote an operator named, by name or by id.
func openRemote(ctx context.Context, st *store.Store, ref string) (store.Remote, error) {
	if strings.TrimSpace(ref) == "" {
		return store.Remote{}, fmt.Errorf("missing -remote: the name or id of the remote, as `jcc-mirror remotes` lists them")
	}
	return st.FindRemote(ctx, ref)
}

// recordRemoteChange is recordPairChange for a remote. The password stays out
// of the event: the log is shown on the dashboard, which asks nobody who they are.
func recordRemoteChange(ctx context.Context, st *store.Store, r store.Remote, what string) {
	err := st.AppendEvent(ctx, &store.Event{
		Level:   store.LevelInfo,
		Kind:    store.KindRemoteChanged,
		Message: fmt.Sprintf("remote %q %s from the command line", r.Name, what),
		Data: map[string]any{
			"action": what, "id": r.ID, "name": r.Name, "host": r.Host, "share": r.Share,
			"path": r.Path, "user": r.User, "domain": r.Domain, "passwordSet": r.PasswordSet,
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: the change was made but not recorded in the event log: %v\n", err)
	}
}

// remoteTarget names a remote the way Windows and the SMB client's Target do.
func remoteTarget(r store.Remote) string {
	out := `\\` + r.Host + `\` + r.Share
	if r.Path != "" {
		out += `\` + strings.ReplaceAll(r.Path, "/", `\`)
	}
	return out
}

func remoteAccount(r store.Remote) string {
	if r.Domain != "" {
		return r.Domain + `\` + r.User
	}
	return r.User
}

func passwordText(r store.Remote) string {
	if r.PasswordSet {
		return "password stored"
	}
	return "no password"
}
