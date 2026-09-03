package wg

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/netip"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// Ping sends one ICMP echo through the tunnel and waits for its reply. It is the
// first M0 check: a reply from the other spoke proves the rootserver forwards
// between clients, which is the single most likely reason for the whole design to
// fail and is not a jcc-mirror bug when it does (DESIGN.md §2.2).
func (t *Tunnel) Ping(ctx context.Context, target netip.Addr, timeout time.Duration) (time.Duration, error) {
	conn, err := t.net.DialPingAddr(netip.Addr{}, target)
	if err != nil {
		return 0, fmt.Errorf("open icmp socket to %s: %w", target, err)
	}
	defer conn.Close()

	var (
		msgType icmp.Type = ipv4.ICMPTypeEcho
		proto             = 1 // IANA protocol number for ICMP
	)
	if !target.Is4() {
		msgType, proto = ipv6.ICMPTypeEchoRequest, 58
	}

	payload := make([]byte, 32)
	if _, err := rand.Read(payload); err != nil {
		return 0, fmt.Errorf("build icmp payload: %w", err)
	}
	seq := int(payload[0])<<8 | int(payload[1])

	req := icmp.Echo{Seq: seq, Data: payload}
	buf, err := (&icmp.Message{Type: msgType, Code: 0, Body: &req}).Marshal(nil)
	if err != nil {
		return 0, fmt.Errorf("marshal icmp echo: %w", err)
	}

	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, fmt.Errorf("set icmp deadline: %w", err)
	}

	start := time.Now()
	if _, err := conn.Write(buf); err != nil {
		return 0, fmt.Errorf("send icmp echo to %s: %w", target, err)
	}

	// The netstack ping socket rewrites the echo id, so the reply is matched on the
	// sequence number and the random payload instead.
	reply := make([]byte, 1500)
	for {
		n, err := conn.Read(reply)
		if err != nil {
			return 0, fmt.Errorf("no icmp reply from %s: %w", target, err)
		}
		msg, err := icmp.ParseMessage(proto, reply[:n])
		if err != nil {
			continue
		}
		echo, ok := msg.Body.(*icmp.Echo)
		if !ok || echo.Seq != seq || !bytes.Equal(echo.Data, payload) {
			continue
		}
		return time.Since(start), nil
	}
}
