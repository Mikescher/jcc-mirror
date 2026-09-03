package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"time"

	"blackforestbytes.com/jcc-mirror/app"
	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/store"
)

// cmdServe is the daemon: the sqlite state, the tunnel it is configured from and
// the dashboard that configures it. It is what the container runs.
//
// The LAN listener does not depend on the tunnel, which is what closes the
// bootstrap loop: a first boot against an empty database answers on :8080 with the
// setup view, and the tunnel comes up the moment the peer entry is saved. Nothing
// has to be known before the process starts (DESIGN.md §6).
func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dataDir := fs.String("data", staticDataDir, "data directory: sqlite state, the WireGuard identity, backups")
	lan := fs.String("lan", staticLANListen, "dashboard address on the host network")
	tunnelPort := fs.Int("tunnel-port", staticTunnelPort, "dashboard port inside the tunnel")
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

	a := app.New(st, logger, app.Options{Version: version, BuildStamp: buildStamp, TunnelPort: *tunnelPort})
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

	<-ctx.Done()
	logger.Infof("shutting down")

	// The shutdown context is deliberately not derived from ctx: it has already
	// been cancelled, and the shutdown still has to record why it happened.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = srv.Shutdown(shutdownCtx)
	a.Close(shutdownCtx)
	return err
}
