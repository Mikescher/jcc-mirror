package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"blackforestbytes.com/jcc-mirror/app"
	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/store"
	"blackforestbytes.com/jcc-mirror/update"
)

// cmdServe is the daemon: the sqlite state, the tunnel it is configured from and
// the dashboard that configures it. It is what the container runs.
//
// The LAN listener does not depend on the tunnel, which is what closes the
// bootstrap loop: a first boot against an empty database answers on :8080 with the
// dashboard, and the tunnel comes up the moment the peer entry is saved. Nothing
// has to be known before the process starts (DESIGN.md §6).
func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dataDir := fs.String("data", staticDataDir, "data directory: sqlite state, the WireGuard identity, backups")
	lan := fs.String("lan", staticLANListen, "dashboard address on the host network")
	tunnelPort := fs.Int("tunnel-port", staticTunnelPort, "dashboard port inside the tunnel")
	devUI := fs.String("dev-ui", "", "proxy the dashboard to a running `ng serve` instead of the build in the binary, e.g. http://localhost:4200")
	remoteDir := fs.String("remote-dir", "", "serve this local directory as the publisher's share instead of opening an SMB session, for development and testing")
	verbose := fs.Bool("v", false, "verbose output, including the wireguard-go device log")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := &logs.Logger{Verbose: *verbose}
	logger.Infof("jcc-mirror %s (built %s), data in %s", version, buildStamp, *dataDir)

	st, err := store.Open(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	a := app.New(st, logger, app.Options{
		Version: version, BuildStamp: buildStamp, TunnelPort: *tunnelPort,
		DataDir: *dataDir, DevUI: *devUI, RemoteDir: *remoteDir,
	})
	handler := a.Handler()
	a.SetHandler(handler)

	if err := a.Start(ctx); err != nil {
		return err
	}

	ln, err := net.Listen("tcp", *lan)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", *lan, err)
	}
	logger.Infof("dashboard: http://%s (on the host network)", ln.Addr())

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Errorf("dashboard: lan listener: %v", err)
		}
	}()

	// Two ways out: the container stopping, and a self-update asking to be
	// restarted into. The second is why the exec is here rather than in the
	// updater - everything has to be shut down first, and the listeners in
	// particular, or the new process cannot bind the addresses this one holds
	// (DESIGN.md §5).
	restart, restarting := "", false
	select {
	case <-ctx.Done():
		logger.Infof("shutting down")
	case restart = <-a.Restart():
		restarting = true
		logger.Infof("restarting after an update")
	}

	// The event streams are told to end before the listener is drained: each one
	// is a request that finishes only when the browser goes away, and Shutdown
	// would wait out its whole timeout for one. The request that asked for the
	// restart still gets to answer.
	a.BeginShutdown()

	// The shutdown context is deliberately not derived from ctx: it has already
	// been cancelled, and the shutdown still has to record why it happened.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)

	// And a second one of its own, because a slow drain must not leave the
	// shutdown with no time left to write itself down.
	closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelClose()
	a.Close(closeCtx)

	if !restarting {
		return err
	}

	// Neither the exec nor the exit below runs a deferred close, so the store is
	// closed here. Closing twice is harmless.
	if err := st.Close(); err != nil {
		logger.Errorf("close the database: %v", err)
	}

	if restart == "" {
		// The binary to run is the one in the image, whose path this process has no
		// way to know. Exiting hands the choice back to the supervisor, which does.
		logger.Infof("update: the binary in the image is what should run now; exiting for the supervisor")
		os.Exit(update.ExitRestart)
	}

	logger.Infof("update: exec %s", restart)
	return update.Exec(restart, os.Args[1:])
}
