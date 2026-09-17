package main

import (
	"context"
	"fmt"

	"blackforestbytes.com/jcc-mirror/app"
	"blackforestbytes.com/jcc-mirror/store"
)

// cmdPassword is the way back in. The dashboard masks its own password like every
// secret and the daemon prints it only to the container log, so a log that has
// rolled over is the one situation where the setting is unreachable from
// everywhere it is normally read. This reads it straight out of sqlite, which
// needs neither the daemon, the tunnel nor the network.
func cmdPassword(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("password")
	reset := fs.Bool("reset", false, "replace it with a newly generated one")
	set := fs.String("set", "", "replace it with this one")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *reset && *set != "" {
		return fmt.Errorf("-reset and -set do the same job; pass one of them")
	}

	st, err := cfg.openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	if *reset || *set != "" {
		pw := *set
		if *reset {
			if pw, err = app.NewPassword(); err != nil {
				return err
			}
		}
		if _, err := st.ConfigSet(ctx, map[string]string{store.KeyDashboardPassword: pw}, "cli"); err != nil {
			return err
		}
		fmt.Printf("%s\n\nEvery browser is logged out. The daemon reads it per request, so a\nrunning one needs no restart.\n", pw)
		return nil
	}

	pw, err := st.ConfigGet(ctx, store.KeyDashboardPassword)
	if err != nil {
		return err
	}
	if pw == "" {
		return fmt.Errorf("no dashboard password is set; `password -reset` makes one")
	}
	fmt.Println(pw)
	return nil
}
