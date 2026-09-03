package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"blackforestbytes.com/jcc-mirror/wg"
)

// cmdServe is M0 step 6: a listener on the tunnel itself, so the publisher can
// confirm he reaches it. This is the property that makes the dashboard free -
// tnet.ListenTCP puts it inside WireGuard, with no port forward, no host-side
// WireGuard and nothing configured on the subscriber's router (DESIGN.md §2.2).
//
// It also closes the loop the ping opened: the ping proves the rootserver routes
// subscriber -> publisher, this proves it routes publisher -> subscriber, and
// both directions are needed.
func cmdServe(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("serve")
	port := fs.Int("port", 8080, "port to listen on inside the tunnel")
	lan := fs.String("lan", "", "additionally listen on this host address, e.g. :8080")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if cfg.noTunnel {
		return fmt.Errorf("serve is about reachability over the tunnel; drop -no-tunnel")
	}

	logger := &Logger{Verbose: cfg.verbose}
	tun, err := cfg.openTunnel(logger)
	if err != nil {
		return err
	}
	defer tun.Close()

	srv := &http.Server{Handler: statusHandler(tun)}

	tunLn, err := tun.Net().ListenTCP(&net.TCPAddr{Port: *port})
	if err != nil {
		return fmt.Errorf("listen on the tunnel: %w", err)
	}
	for _, addr := range tun.Addrs() {
		logger.Infof("serve: http://%s:%d (inside the tunnel)", addr, *port)
	}
	go serve(logger, srv, tunLn, "tunnel")

	if *lan != "" {
		lanLn, err := net.Listen("tcp", *lan)
		if err != nil {
			return fmt.Errorf("listen on %q: %w", *lan, err)
		}
		logger.Infof("serve: http://%s (on the host network)", lanLn.Addr())
		go serve(logger, srv, lanLn, "lan")
	}

	<-ctx.Done()
	logger.Infof("serve: shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func serve(logger *Logger, srv *http.Server, ln net.Listener, name string) {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Errorf("serve: %s listener: %v", name, err)
	}
}

// statusHandler answers with the same facts the diagnostics view will later show,
// in a shape that is readable from a phone browser over the tunnel.
func statusHandler(tun *wg.Tunnel) http.Handler {
	started := time.Now()
	host, _ := os.Hostname()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		fmt.Fprintf(w, "jcc-mirror M0 spike\n\n")
		fmt.Fprintf(w, "host        %s\n", host)
		fmt.Fprintf(w, "time        %s\n", time.Now().Format(time.RFC3339))
		fmt.Fprintf(w, "uptime      %s\n", humanDuration(time.Since(started)))
		fmt.Fprintf(w, "you are     %s\n", r.RemoteAddr)
		fmt.Fprintf(w, "endpoint    %s\n", tun.Endpoint())

		st, err := tun.Status()
		if err != nil {
			fmt.Fprintf(w, "\nwireguard status unavailable: %v\n", err)
			return
		}
		fmt.Fprintf(w, "\n%d peer(s):\n", len(st.Peers))
		for _, p := range st.Peers {
			hs := "never"
			if age, ok := p.HandshakeAge(); ok {
				hs = humanDuration(age) + " ago"
			}
			fmt.Fprintf(w, "  %s via %s, handshake %s, tx %s, rx %s\n",
				p.PublicKey, p.Endpoint, hs, humanBytes(p.TxBytes), humanBytes(p.RxBytes))
		}
	})
}
