package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/store"
	"blackforestbytes.com/jcc-mirror/update"
)

// cmdUpdate is the self-updater from a shell rather than from the dashboard, on
// the same state the daemon runs on. The two halves that never touch the network
// - what is installed, and stepping back off it - are what it is really for: the
// case a rollback exists for is one where the dashboard may be the broken thing.
//
// -check and -apply fetch the binary URL directly, exactly as the daemon does.
func cmdUpdate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	dataDir := fs.String("data", staticDataDir, "data directory: sqlite state, and bin/ where the binaries live")
	check := fs.Bool("check", false, "ask the update URL what it has; the only flag that touches the network on its own")
	apply := fs.Bool("apply", false, "install the binary from the update URL, if it is newer")
	force := fs.Bool("force", false, "install it even when it is not newer, or was rolled back before")
	rollback := fs.Bool("rollback", false, "put the previous binary back, so the next start runs it")
	if err := fs.Parse(args); err != nil {
		return err
	}

	manager := update.NewManager(*dataDir)

	if *rollback {
		res, err := manager.Rollback("rolled back with `jcc-mirror update -rollback`")
		if err != nil {
			return err
		}
		if res.Exec == "" {
			fmt.Println("rolled back to the binary in the image; restart to pick it up")
		} else {
			fmt.Printf("rolled back to %s; restart to pick it up\n", res.Exec)
		}
		return nil
	}

	fmt.Printf("running   jcc-mirror %s, built %s\n", version, buildStamp)
	if state, ok, err := manager.LoadState(); err != nil {
		return err
	} else if ok {
		fmt.Printf("installed %s at %s, %s\n", state.State,
			state.UpdatedAt.Local().Format(time.RFC3339), manager.Binary())
		fmt.Printf("          %s replaced %s\n", state.ToVersion, state.FromVersion)
		if state.RollbackReason != "" {
			fmt.Printf("          %s\n", state.RollbackReason)
		}
	} else {
		fmt.Println("installed nothing; this is the binary the image shipped")
	}

	if !*check && !*apply {
		return nil
	}

	st, err := store.Open(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	values, err := st.Config(ctx)
	if err != nil {
		return err
	}
	src, err := updateSourceOf(values)
	if err != nil {
		return err
	}

	rel, err := src.Head(ctx)
	if err != nil {
		return err
	}

	baseline := manager.Baseline(update.ParseBuildStamp(buildStamp))
	newer := rel.ModTime.After(baseline)

	fmt.Printf("server    %s\n", rel.URL)
	fmt.Printf("          %s, %s\n", rel.ModTime.Format(time.RFC3339), format.Bytes(rel.Size))
	if newer {
		fmt.Println("          newer than what is running")
	} else {
		fmt.Printf("          not newer than %s\n", baseline.Format(time.RFC3339))
	}

	blocked, _ := manager.Blocked()
	if blocked.State != "" && !rel.ModTime.After(blocked.RemoteModTime) {
		fmt.Printf("blocked   %s\n", blocked.RollbackReason)
		if *apply && !*force {
			return errors.New("that binary was rolled back once already; -force installs it anyway")
		}
	}

	if !*apply {
		return nil
	}
	if !newer && !*force {
		return errors.New("nothing to do: the binary at the update URL is not newer; -force installs it anyway")
	}

	fmt.Printf("fetching  %s\n", rel.URL)
	res, err := manager.Apply(ctx, src, rel, version)
	if err != nil {
		return err
	}
	fmt.Printf("installed %s, built %s, at %s\n", res.State.ToVersion, res.State.ToBuild, res.Exec)
	fmt.Println("          restart to run it; the supervisor puts this one back if it will not start")
	return nil
}

// updateSourceOf reads the binary URL the daemon would use.
func updateSourceOf(values store.Values) (update.Source, error) {
	rawURL, err := update.Locate(values.Get(store.KeyUpdateURL))
	if err != nil {
		return update.Source{}, err
	}
	return update.Source{URL: rawURL}, nil
}
