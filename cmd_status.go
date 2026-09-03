package main

import (
	"context"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/webdav"
	"blackforestbytes.com/jcc-mirror/wg"
)

// cmdStatus is M0 step 2: join from the container and see the tunnel come up.
// It reports what a dashboard diagnostics view will later report - handshake age,
// endpoint, byte counters - and optionally touches the WebDAV root, which is the
// smallest end-to-end proof that the whole path works.
func cmdStatus(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("status")
	wait := fs.Duration("wait", 15*time.Second, "how long to wait for the first handshake")
	watch := fs.Duration("watch", 0, "repeat at this interval instead of exiting once")
	probe := fs.Bool("probe", true, "also PROPFIND the WebDAV root when -url is given")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := &logs.Logger{Verbose: cfg.verbose}

	if cfg.wgPrivateKey != "" {
		pub, err := wg.PublicKey(cfg.wgPrivateKey)
		if err != nil {
			return err
		}
		logger.Infof("wg: our public key is %s", pub)
	}

	tun, err := cfg.openTunnel(ctx, logger)
	if err != nil {
		return err
	}
	if tun != nil {
		defer tun.Close()

		hsCtx, cancel := context.WithTimeout(ctx, *wait)
		if err := tun.WaitHandshake(hsCtx); err != nil {
			logger.Errorf("wg: %v", err)
		}
		cancel()
	}

	var client *webdav.Client
	if *probe && cfg.davURL != "" {
		client, err = cfg.openDAV(ctx, tun)
		if err != nil {
			return err
		}
	}

	for {
		if tun != nil {
			if err := printTunnelStatus(logger, tun); err != nil {
				return err
			}
		}
		if client != nil {
			probeRoot(ctx, logger, client)
		}

		if *watch <= 0 || !sleepCtx(ctx, *watch) {
			return nil
		}
	}
}

func printTunnelStatus(logger *logs.Logger, tun *wg.Tunnel) error {
	st, err := tun.Status()
	if err != nil {
		return err
	}

	logger.Infof("wg: listening on udp/%d, %d peer(s)", st.ListenPort, len(st.Peers))
	for _, p := range st.Peers {
		hs := "never"
		if age, ok := p.HandshakeAge(); ok {
			hs = format.Duration(age) + " ago"
		}
		logger.Infof("wg: peer %s via %s, handshake %s, tx %s, rx %s, allowed %v",
			p.PublicKey, p.Endpoint, hs, format.Bytes(p.TxBytes), format.Bytes(p.RxBytes), p.AllowedIPs)
	}
	return nil
}

// probeRoot lists the remote root once. Failure is reported rather than returned:
// the tunnel status above is still worth seeing when WebDAV is the broken half.
func probeRoot(ctx context.Context, logger *logs.Logger, client *webdav.Client) {
	start := time.Now()
	entries, err := client.List(ctx, "")
	if err != nil {
		logger.Errorf("webdav: %s: %v", client.BaseURL(), err)
		return
	}

	var dirs, files int
	for _, e := range entries {
		if e.IsDir {
			dirs++
		} else {
			files++
		}
	}
	logger.Infof("webdav: %s -> %d dirs, %d files in %s", client.BaseURL(), dirs, files, format.Duration(time.Since(start)))
}
