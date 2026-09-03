// Package wg brings up a WireGuard tunnel that exists only inside this process:
// wireguard-go driving a gVisor userspace TCP/IP stack. Nothing is created on the
// host, so the container needs no NET_ADMIN, no --privileged and no /dev/net/tun,
// and the WireGuard identity is a file in the data volume rather than host state
// (DESIGN.md §2.2).
package wg

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// Defaults applied by Open when the corresponding Config field is zero.
const (
	// DefaultMTU is the usual cause of "WireGuard is mysteriously slow" when it is
	// wrong, so it is spelled out rather than left to the caller.
	DefaultMTU = 1420

	// DefaultKeepalive keeps the NAT mapping open. Both ends of this topology sit
	// behind NAT, so without it the tunnel only works in the direction that spoke
	// happened to send last.
	DefaultKeepalive = 25
)

// Config describes the tunnel. The string fields take the same shapes as a
// wg-quick config file, so values can be copied across unchanged.
type Config struct {
	PrivateKey    string // base64
	PeerPublicKey string // base64
	PresharedKey  string // base64, optional
	Endpoint      string // host:port of the peer; a hostname is resolved once, see Open
	Addresses     string // comma-separated tunnel addresses, with or without a /prefix
	AllowedIPs    string // comma-separated CIDRs routed into the tunnel
	DNS           string // comma-separated resolvers reachable through the tunnel, optional
	Keepalive     int    // seconds, -1 disables
	MTU           int
	ListenPort    int                           // 0 picks a random source port
	Logf          func(format string, a ...any) // wireguard-go device log, nil discards it
}

// Tunnel is a running WireGuard device with its own network stack.
type Tunnel struct {
	dev      *device.Device
	net      *netstack.Net
	endpoint netip.AddrPort
	addrs    []netip.Addr
}

// Open brings the tunnel up. The returned Tunnel is usable immediately, but the
// first handshake has not necessarily happened yet - use WaitHandshake when that
// matters.
func Open(cfg Config) (*Tunnel, error) {
	addrs, err := parseAddrs(cfg.Addresses)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("wireguard: no tunnel address configured")
	}
	allowed, err := parsePrefixes(cfg.AllowedIPs)
	if err != nil {
		return nil, err
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("wireguard: no allowed-ips configured")
	}
	dns, err := parseAddrs(cfg.DNS)
	if err != nil {
		return nil, err
	}

	endpoint, err := resolveEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}

	uapi, err := uapiConfig(cfg, endpoint, allowed)
	if err != nil {
		return nil, err
	}

	mtu := cfg.MTU
	if mtu == 0 {
		mtu = DefaultMTU
	}

	tun, tnet, err := netstack.CreateNetTUN(addrs, dns, mtu)
	if err != nil {
		return nil, fmt.Errorf("create netstack tun: %w", err)
	}

	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	dev := device.NewDevice(tun, conn.NewDefaultBind(), &device.Logger{Verbosef: logf, Errorf: logf})

	if err := dev.IpcSet(uapi); err != nil {
		dev.Close()
		return nil, fmt.Errorf("configure wireguard device: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("bring wireguard device up: %w", err)
	}

	return &Tunnel{dev: dev, net: tnet, endpoint: endpoint, addrs: addrs}, nil
}

// Close tears the tunnel down. There is nothing to report: the device owns only
// in-process state.
func (t *Tunnel) Close() { t.dev.Close() }

// Net exposes the userspace network stack, for listeners and non-HTTP dialling.
func (t *Tunnel) Net() *netstack.Net { return t.net }

// Endpoint returns the resolved peer endpoint actually configured.
func (t *Tunnel) Endpoint() netip.AddrPort { return t.endpoint }

// Addrs returns the tunnel's own addresses.
func (t *Tunnel) Addrs() []netip.Addr { return t.addrs }

// DialContext dials through the tunnel.
func (t *Tunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return t.net.DialContext(ctx, network, address)
}

// Transport returns an http.Transport whose connections go through the tunnel.
// This is the entire integration surface between WireGuard and WebDAV.
func (t *Tunnel) Transport() *http.Transport {
	return &http.Transport{
		DialContext:         t.net.DialContext,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 16, // a walk keeps several PROPFINDs in flight at once
		IdleConnTimeout:     90 * time.Second,
	}
}

// WaitHandshake blocks until a peer has completed a handshake, or ctx ends. A
// tunnel that never handshakes is the failure the M0 spike exists to catch, and
// the most likely cause is the rootserver not forwarding between spokes.
func (t *Tunnel) WaitHandshake(ctx context.Context) error {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()

	for {
		st, err := t.Status()
		if err != nil {
			return err
		}
		for _, p := range st.Peers {
			if !p.LastHandshake.IsZero() {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("no wireguard handshake: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// PublicKey derives the base64 public key belonging to a base64 private key. It
// is what has to be pasted into the rootserver's peer entry for this container.
func PublicKey(privateKey string) (string, error) {
	raw, err := decodeKey(privateKey, "private key")
	if err != nil {
		return "", err
	}
	pub, err := curve25519.X25519(raw, curve25519.Basepoint)
	if err != nil {
		return "", fmt.Errorf("derive public key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(pub), nil
}

// uapiConfig renders the wireguard-go IPC configuration. Keys go over that
// interface as hex, not as the base64 of a wg-quick file.
func uapiConfig(cfg Config, endpoint netip.AddrPort, allowed []netip.Prefix) (string, error) {
	priv, err := hexKey(cfg.PrivateKey, "private key")
	if err != nil {
		return "", err
	}
	pub, err := hexKey(cfg.PeerPublicKey, "peer public key")
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", priv)
	if cfg.ListenPort > 0 {
		fmt.Fprintf(&b, "listen_port=%d\n", cfg.ListenPort)
	}
	fmt.Fprintf(&b, "public_key=%s\n", pub)
	if strings.TrimSpace(cfg.PresharedKey) != "" {
		psk, err := hexKey(cfg.PresharedKey, "preshared key")
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "preshared_key=%s\n", psk)
	}
	fmt.Fprintf(&b, "endpoint=%s\n", endpoint.String())
	if cfg.Keepalive >= 0 {
		ka := cfg.Keepalive
		if ka == 0 {
			ka = DefaultKeepalive
		}
		fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", ka)
	}
	for _, p := range allowed {
		fmt.Fprintf(&b, "allowed_ip=%s\n", p.String())
	}
	return b.String(), nil
}

// resolveEndpoint turns host:port into ip:port on the host network, before the
// tunnel exists. wireguard-go parses the endpoint with netip.ParseAddrPort and
// nothing else: a hostname is rejected outright, and the address is resolved once
// and never again. The design puts a fixed rootserver IP here for that reason.
func resolveEndpoint(s string) (netip.AddrPort, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.AddrPort{}, fmt.Errorf("wireguard: no endpoint configured")
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap, nil
	}

	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("parse endpoint %q: %w", s, err)
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("parse endpoint %q: bad port: %w", s, err)
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("resolve endpoint %q: %w", s, err)
	}
	for _, fam := range []bool{true, false} { // IPv4 first: UDP over IPv6 is the rarer path here
		for _, ip := range ips {
			addr, ok := netip.AddrFromSlice(ip)
			if !ok {
				continue
			}
			addr = addr.Unmap()
			if addr.Is4() == fam {
				return netip.AddrPortFrom(addr, uint16(p)), nil
			}
		}
	}
	return netip.AddrPort{}, fmt.Errorf("resolve endpoint %q: no usable address", s)
}

// parseAddrs parses a comma-separated address list. A /prefix is accepted and
// dropped, so a wg-quick "Address = 10.0.0.3/32" can be pasted in unchanged.
func parseAddrs(s string) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, f := range splitList(s) {
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Addr())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("parse address %q: %w", f, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// parsePrefixes parses a comma-separated CIDR list; a bare address is taken as a
// host route.
func parsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range splitList(s) {
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("parse allowed-ip %q: %w", f, err)
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func decodeKey(s, name string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("decode %s: %d bytes, want 32", name, len(raw))
	}
	return raw, nil
}

func hexKey(s, name string) (string, error) {
	raw, err := decodeKey(s, name)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
