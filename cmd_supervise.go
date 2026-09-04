package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/update"
)

// cmdSupervise is the container's entrypoint: it runs the daemon as a child and
// puts the previous binary back if a fresh self-update will not start.
//
// It is the whole trust story of DESIGN.md §5 together with the download checks -
// aimed at corruption rather than tampering, because a broken updater is the one
// bug that cannot be fixed from the dashboard it broke.
func cmdSupervise(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("supervise", flag.ExitOnError)
	dataDir := fs.String("data", staticDataDir, "data directory; the updated binaries live in its bin/ subdirectory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	child := fs.Args()
	if len(child) == 0 {
		child = []string{"serve"}
	}

	// The image's binary, and the fallback a rollback lands on. os.Executable is
	// the only thing that knows it: the path is the ENTRYPOINT's, which is not a
	// setting and not something the daemon could be told.
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find this binary: %w", err)
	}

	logger := &logs.Logger{}
	sv := update.Supervisor{
		Manager:  update.NewManager(*dataDir),
		Fallback: self,
		Args:     child,
		Logf:     logger.Infof,
	}

	code, err := sv.Run(ctx)
	if err != nil {
		return err
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}
