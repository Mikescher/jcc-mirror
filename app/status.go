package app

import (
	"context"
	"time"

	"blackforestbytes.com/jcc-mirror/schedule"
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
	Version    string         `json:"version"`
	BuildStamp string         `json:"buildStamp,omitempty"`
	StartedAt  time.Time      `json:"startedAt"`
	UptimeSec  int64          `json:"uptimeSeconds"`
	Database   string         `json:"database"`
	PublicKey  string         `json:"publicKey,omitempty"`
	Tunnel     TunnelStatus   `json:"tunnel"`
	Remotes    []RemoteStatus `json:"remotes"`
	Schedule   ScheduleStatus `json:"schedule"`
	// UpdateReady is the badge in the topbar. The rest of the update panel is a
	// view of its own: this is the only part of it worth carrying on every frame
	// of the live stream.
	UpdateReady bool `json:"updateReady,omitempty"`
}

// ScheduleStatus is what the grid says right now, in the zone it is expressed
// in. NextOpen is only filled when transfers are closed, which is the one moment
// anyone wants to know it.
type ScheduleStatus struct {
	Timezone  string          `json:"timezone"`
	Now       time.Time       `json:"now"`
	Automatic bool            `json:"automatic"`
	ScanEvery string          `json:"scanEvery"`
	Transfer  schedule.Window `json:"transfer"`
	Scan      schedule.Window `json:"scan"`
	NextOpen  *time.Time      `json:"nextOpen,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// Describe is the transfer window in one line, which is what the page and the
// CLI both print.
func (s ScheduleStatus) Describe() string { return s.Transfer.Describe() }

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

// RemoteStatus is one stored remote as the status reports it. Target is taken
// from the client rather than from the row, so what is reported is the target
// the mirror would actually read, port defaults and all.
type RemoteStatus struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Target string `json:"target,omitempty"`
	Error  string `json:"error,omitempty"`
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

	st.Remotes = a.remoteStatuses(ctx)
	st.Schedule = a.ScheduleStatus()
	st.UpdateReady = a.updateReady()

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

func (a *App) remoteStatuses(ctx context.Context) []RemoteStatus {
	out := []RemoteStatus{}
	remotes, err := a.store.Remotes(ctx)
	if err != nil {
		a.log.Errorf("status: %v", err)
		return out
	}

	if len(remotes) == 0 && a.opts.RemoteDir != "" {
		remotes = []store.Remote{{Name: localRemoteName}}
	}
	for _, r := range remotes {
		rs := RemoteStatus{ID: r.ID, Name: r.Name}
		var err error
		if rs.Target, err = a.remoteState(r.ID); err != nil {
			rs.Error = err.Error()
		}
		out = append(out, rs)
	}
	return out
}

// ScheduleStatus answers the grid at this moment. It is separate from Status so
// the schedule view can be drawn without collecting the tunnel's counters.
func (a *App) ScheduleStatus() ScheduleStatus {
	a.sched.mu.Lock()
	cfg, cfgErr := a.sched.cfg, a.sched.cfgErr
	a.sched.mu.Unlock()

	// cfg is already in hand, so the zone comes from it rather than from a second
	// acquisition that could straddle a reload and answer for a different config.
	loc := cfg.loc
	if loc == nil {
		loc = time.Local
	}
	now := time.Now().In(loc)

	out := ScheduleStatus{
		Timezone:  loc.String(),
		Now:       now,
		Automatic: cfg.automatic,
		ScanEvery: cfg.scanEvery.String(),
		Transfer:  cfg.transfer.At(now),
		Scan:      cfg.scan.At(now),
		Error:     cfgErr,
	}
	if !out.Transfer.Open {
		if next, ok := cfg.transfer.NextOpen(now); ok {
			out.NextOpen = &next
		}
	}
	return out
}

// Schedules hands out the grids themselves, for the view that draws them.
func (a *App) Schedules() (transfer, scan schedule.Schedule) {
	a.sched.mu.Lock()
	defer a.sched.mu.Unlock()
	return a.sched.cfg.transfer, a.sched.cfg.scan
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
