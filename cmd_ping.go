package main

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/logs"
)

// cmdPing is M0 step 1: an ICMP echo to the publisher's WireGuard address.
//
// A reply proves the rootserver forwards between its spokes, which is the single
// most likely reason for the rest of the design to fail and is not a jcc-mirror
// bug when it does. Nothing further is worth trying until this passes.
func cmdPing(ctx context.Context, args []string) error {
	fs, cfg := newFlagSet("ping")
	target := fs.String("target", "", "the publisher's address inside the tunnel")
	count := fs.Int("count", 5, "number of echo requests")
	interval := fs.Duration("interval", time.Second, "delay between echo requests")
	timeout := fs.Duration("timeout", 5*time.Second, "per-request timeout")
	handshake := fs.Duration("handshake-timeout", 15*time.Second, "how long to wait for the first handshake")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *target == "" {
		return fmt.Errorf("missing -target: the publisher's WireGuard address")
	}
	addr, err := netip.ParseAddr(*target)
	if err != nil {
		return fmt.Errorf("parse -target %q: %w", *target, err)
	}
	if cfg.noTunnel {
		return fmt.Errorf("ping goes through the tunnel by definition; drop -no-tunnel")
	}

	logger := &logs.Logger{Verbose: cfg.verbose}
	tun, err := cfg.openTunnel(ctx, logger)
	if err != nil {
		return err
	}
	defer tun.Close()

	hsCtx, cancel := context.WithTimeout(ctx, *handshake)
	defer cancel()
	if err := tun.WaitHandshake(hsCtx); err != nil {
		return fmt.Errorf("%w - the endpoint is unreachable, the keys do not match, or UDP to the rootserver is blocked", err)
	}
	logger.Infof("wg: handshake complete with %s", tun.Endpoint())

	var (
		rtts  []time.Duration
		total time.Duration
		lo    = time.Duration(1<<63 - 1)
		hi    time.Duration
	)
	for i := 0; i < *count && ctx.Err() == nil; i++ {
		if i > 0 && !sleepCtx(ctx, *interval) {
			break
		}
		rtt, err := tun.Ping(ctx, addr, *timeout)
		if err != nil {
			logger.Errorf("ping %s seq %d: %v", addr, i+1, err)
			continue
		}
		logger.Infof("ping %s seq %d: %s", addr, i+1, format.Duration(rtt))
		rtts = append(rtts, rtt)
		total += rtt
		lo = min(lo, rtt)
		hi = max(hi, rtt)
	}

	sent := *count
	logger.Infof("--- %s ping statistics ---", addr)
	logger.Infof("%d sent, %d received, %d%% loss", sent, len(rtts), 100*(sent-len(rtts))/max(sent, 1))
	if len(rtts) > 0 {
		logger.Infof("rtt min/avg/max = %s / %s / %s", format.Duration(lo), format.Duration(total/time.Duration(len(rtts))), format.Duration(hi))
	}

	if len(rtts) == 0 {
		return fmt.Errorf("no replies from %s: the handshake with the rootserver works, so the tunnel is fine and the route between spokes is not. "+
			"Check net.ipv4.ip_forward=1 on the rootserver and that its peer entries' AllowedIPs cover both clients (DESIGN.md §2.2)", addr)
	}
	return nil
}

// sleepCtx waits for d, or returns false when the context ends first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
