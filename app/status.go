package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	"blackforestbytes.com/jcc-mirror/store"
	"blackforestbytes.com/jcc-mirror/wg"
)

// Tunnel states, in the order a first boot goes through them.
const (
	TunnelUnconfigured = "unconfigured"
	TunnelFailed       = "failed"
	TunnelConnecting   = "connecting"
	TunnelUp           = "up"
)

// Status is what /healthz answers and what the dashboard renders. It is the
// shape the diagnostics view will keep using (DESIGN.md §4).
type Status struct {
	Version    string       `json:"version"`
	BuildStamp string       `json:"buildStamp,omitempty"`
	StartedAt  time.Time    `json:"startedAt"`
	UptimeSec  int64        `json:"uptimeSeconds"`
	Database   string       `json:"database"`
	PublicKey  string       `json:"publicKey,omitempty"`
	Tunnel     TunnelStatus `json:"tunnel"`
	Remote     RemoteStatus `json:"remote"`
}

type TunnelStatus struct {
	State      string   `json:"state"`
	Error      string   `json:"error,omitempty"`
	Endpoint   string   `json:"endpoint,omitempty"`
	Addresses  []string `json:"addresses,omitempty"`
	ListenPort int      `json:"listenPort,omitempty"`
	Peers      []Peer   `json:"peers,omitempty"`
}

type Peer struct {
	PublicKey    string   `json:"publicKey"`
	Endpoint     string   `json:"endpoint,omitempty"`
	HandshakeAge *float64 `json:"handshakeAgeSeconds,omitempty"`
	TxBytes      int64    `json:"txBytes"`
	RxBytes      int64    `json:"rxBytes"`
	AllowedIPs   []string `json:"allowedIps,omitempty"`
}

type RemoteStatus struct {
	Configured bool   `json:"configured"`
	URL        string `json:"url,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Healthy reports whether /healthz should answer 200. A tunnel that is down is
// not unhealthy: a first boot has no tunnel by definition, and restarting the
// container would not help. A database that has stopped answering is, because
// the volume going away underneath a running container is a real NAS failure and
// nothing else the process reports would look wrong.
func (s Status) Healthy() bool { return s.Database == "ok" }

// Status collects everything the dashboard and /healthz report.
func (a *App) Status(ctx context.Context) Status {
	st := Status{
		Version:    a.opts.Version,
		BuildStamp: a.opts.BuildStamp,
		StartedAt:  a.started,
		UptimeSec:  int64(time.Since(a.started).Seconds()),
		Database:   "ok",
	}
	if err := a.store.Ping(ctx); err != nil {
		st.Database = err.Error()
	}
	if pub, err := a.PublicKey(ctx); err == nil {
		st.PublicKey = pub
	}

	values, err := a.store.Config(ctx)
	if err == nil {
		st.Remote.URL = values.Get(store.KeyRemoteURL)
		st.Remote.Configured = st.Remote.URL != ""
	}
	if _, err := a.Remote(); err != nil {
		st.Remote.Error = err.Error()
	}

	a.mu.Lock()
	tun, tunErr := a.tunnel, a.tunnelErr
	a.mu.Unlock()

	switch {
	case tun == nil && tunErr != nil:
		st.Tunnel.State = TunnelFailed
		st.Tunnel.Error = tunErr.Error()
	case tun == nil:
		st.Tunnel.State = TunnelUnconfigured
	default:
		st.Tunnel = tunnelStatus(tun)
	}
	return st
}

func tunnelStatus(tun *wg.Tunnel) TunnelStatus {
	out := TunnelStatus{State: TunnelConnecting, Endpoint: tun.Endpoint().String()}
	for _, a := range tun.Addrs() {
		out.Addresses = append(out.Addresses, a.String())
	}

	dev, err := tun.Status()
	if err != nil {
		out.State = TunnelFailed
		out.Error = err.Error()
		return out
	}
	out.ListenPort = dev.ListenPort

	for _, p := range dev.Peers {
		peer := Peer{
			PublicKey:  p.PublicKey,
			Endpoint:   p.Endpoint,
			TxBytes:    p.TxBytes,
			RxBytes:    p.RxBytes,
			AllowedIPs: p.AllowedIPs,
		}
		if age, ok := p.HandshakeAge(); ok {
			secs := age.Seconds()
			peer.HandshakeAge = &secs
			if age < handshakeStale {
				out.State = TunnelUp
			}
		}
		out.Peers = append(out.Peers, peer)
	}
	return out
}

// newToken makes the dashboard bearer token. Base64url so it survives a copy out
// of a container log and into a header without escaping.
func newToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
