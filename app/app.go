// Package app is the running daemon: the sqlite store, the WireGuard tunnel it is
// configured from, the WebDAV client that rides on it, and the dashboard that
// reports on all three.
//
// Everything is driven from the config table, so there is nothing to know before
// the process starts. The LAN dashboard does not depend on the tunnel, so a first
// boot against an empty database answers with an empty Config view, and the
// tunnel comes up the moment the peer entry is saved (DESIGN.md §6).
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"blackforestbytes.com/jcc-mirror/engine"
	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/notify"
	"blackforestbytes.com/jcc-mirror/store"
	"blackforestbytes.com/jcc-mirror/update"
	"blackforestbytes.com/jcc-mirror/webdav"
	"blackforestbytes.com/jcc-mirror/wg"
)

// reconcileInterval is how often the tunnel is re-examined: a handshake that has
// gone stale is noticed, and a tunnel that failed to open - a rootserver that was
// down at boot, say - is retried.
const reconcileInterval = 30 * time.Second

// handshakeStale is how long without a handshake counts as down. WireGuard
// rehandshakes every two minutes under traffic and the keepalive is 25 s, so
// anything past this is not just a quiet link.
const handshakeStale = 3 * time.Minute

// Options are the static settings (DESIGN.md §6) plus the build identity.
type Options struct {
	Version    string
	BuildStamp string
	TunnelPort int
	// DataDir is the volume mount. The engine wants it for the database backups
	// of DESIGN.md S5, which live with the rest of the state rather than beside
	// the pair.
	DataDir string
	// DevUI is a running `ng serve` to proxy the dashboard to instead of serving
	// the build embedded in the binary. Empty in every deployment.
	DevUI string
}

// App owns the daemon's mutable state. Every exported method is safe to call
// from a request handler.
type App struct {
	store *store.Store
	log   *logs.Logger
	opts  Options

	// handler serves both listeners. It is set once, before Start.
	handler http.Handler

	// runs is the one mirror operation that may be in flight, started from the
	// dashboard or by the scheduler. It has a lock of its own: a sync runs for
	// days, and the tunnel paths must not queue behind it.
	runs runner

	// sched is the 7x24 grid and what it has decided; limiter is the cap it
	// carries into whatever is transferring. The limiter belongs to the daemon
	// rather than to a run, because a window boundary has to reach a transfer
	// that is already going (DESIGN.md §2.4, §6).
	sched   scheduler
	limiter *engine.Limiter

	// stream is the SSE fan-out the dashboard is drawn from, meter counts what
	// crosses the wire for the bandwidth series, and notifier is the push channel
	// that makes a stopped sync visible without anyone going to look.
	stream   stream
	meter    meter
	notifier *notify.Client

	// upd is the self-updater: what the last check found, and the binary
	// directory in the data volume that the supervisor reads too. restart carries
	// the binary to exec into, which the daemon's own main loop waits on beside
	// its context - the tear-down has to happen before the exec, and only main
	// can do it (DESIGN.md §5).
	upd     updates
	restart chan string

	// closing is shut the moment a shutdown or a restart begins, so the handlers
	// that never finish on their own - the event stream - end rather than being
	// waited out. Without it every restart costs the shutdown timeout, and the
	// re-exec of DESIGN.md §5 is not the instant thing it is meant to be.
	closing   chan struct{}
	closeOnce sync.Once

	// bg counts the goroutines that outlive the request or tick that started
	// them - a run and the notifications it produces. Close waits on it, because
	// the store is closed the moment Close returns and both of them write to it.
	bg sync.WaitGroup

	mu        sync.Mutex
	baseCtx   context.Context // the daemon's lifetime, which a run's context hangs off
	tunnel    *wg.Tunnel
	tunnelCfg tunnelSettings
	tunnelErr error
	tunnelLn  net.Listener
	tunnelSrv *http.Server
	remote    *webdav.Client
	remoteTr  *http.Transport // held only so a replaced one can let its connections go
	remoteErr error
	wantTun   bool // the tunnel is configured, whether or not it is currently open
	up        bool // last reported tunnel state, so events fire on the edge only
	downSince time.Time
	started   time.Time
}

func New(st *store.Store, log *logs.Logger, opts Options) *App {
	if opts.TunnelPort == 0 {
		opts.TunnelPort = 8080
	}
	a := &App{
		store: st, log: log, opts: opts, started: time.Now(),
		limiter: engine.NewLimiter(0), notifier: &notify.Client{},
		restart: make(chan string, 1), closing: make(chan struct{}),
	}
	a.upd.manager = update.NewManager(opts.DataDir)
	return a
}

// SetHandler installs the dashboard. The tunnel listener is opened and closed as
// the tunnel comes and goes, so the App has to hold the handler rather than the
// other way round.
func (a *App) SetHandler(h http.Handler) { a.handler = h }

func (a *App) Store() *store.Store { return a.store }

// Start generates the settings that are never typed by hand, brings the tunnel up
// if it is configured, and starts the reconcile loop. It does not fail on a
// tunnel that will not open: the dashboard is how that gets fixed.
func (a *App) Start(ctx context.Context) error {
	a.mu.Lock()
	a.baseCtx = ctx
	a.mu.Unlock()

	created, err := a.store.EnsureGenerated(ctx, generateSetting)
	if err != nil {
		return err
	}
	for _, k := range created {
		a.log.Infof("config: generated %s", k)
	}

	token, err := a.store.ConfigGet(ctx, store.KeyDashboardToken)
	if err != nil {
		return err
	}
	// The token is printed rather than stored anywhere a human reads: it exists in
	// exactly one place, and this is how it gets out of it (DESIGN.md §6). Secretf
	// keeps it out of the log ring, which the dashboard serves back.
	a.log.Secretf("dashboard: bearer token is %s", token)

	if pub, err := a.PublicKey(ctx); err == nil {
		a.log.Infof("wg: our public key is %s - paste it into the rootserver's peer entry", pub)
	}

	a.Event(ctx, store.LevelInfo, store.KindStartup, "jcc-mirror started", map[string]any{
		"version": a.opts.Version,
		"build":   a.opts.BuildStamp,
	})

	// Before the loops: a process the supervisor started after a rollback has
	// something to say about the one before it, and it should say it first.
	a.reportUpdate(ctx)

	a.Reload(ctx)
	go a.reconcileLoop(ctx)
	go a.schedulerLoop(ctx)
	go a.feedLoop(ctx)
	go a.maintenanceLoop(ctx)
	go a.updateLoop(ctx)
	return nil
}

// BeginShutdown tells the long-lived handlers to end. It is separate from Close
// because it has to happen before the HTTP server is drained: an event stream is
// a request that finishes only when the client goes away, and Shutdown would
// otherwise wait out its whole timeout for one.
func (a *App) BeginShutdown() {
	a.closeOnce.Do(func() { close(a.closing) })
}

// Close tears the tunnel down and records the shutdown.
func (a *App) Close(ctx context.Context) {
	a.BeginShutdown()

	// A run in flight is stopped rather than left to be killed with the process.
	// Nothing is lost either way - a scan stays resumable and a transfer keeps its
	// watermark - but stopping it means the run's own record gets written.
	_ = a.CancelRun("stopped: the daemon is shutting down")
	a.awaitBackground(3 * time.Second)

	a.Event(ctx, store.LevelInfo, store.KindShutdown, "jcc-mirror stopped", nil)

	a.mu.Lock()
	defer a.mu.Unlock()
	a.closeTunnelLocked()
}

// awaitBackground waits for the run and the notifications it produced to finish
// writing, so their records are in the database before it is closed. It is a
// courtesy with a short fuse: a transfer that will not stop must not hold up the
// shutdown.
//
// Waiting on the runner's state would not do it - a run clears itself before its
// notifications are sent, and those are the writes that would land on a closed
// database.
func (a *App) awaitBackground(limit time.Duration) {
	done := make(chan struct{})
	go func() {
		a.bg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(limit):
		a.log.Warnf("shutdown: something in the background did not finish in %s", limit)
	}
}

// Reload applies the current configuration. It is called at startup and after
// every configuration change; re-opening the tunnel in place is what lets the
// dashboard configure the tunnel it is not reachable through.
func (a *App) Reload(ctx context.Context) {
	values, err := a.store.Config(ctx)
	if err != nil {
		a.log.Errorf("config: %v", err)
		return
	}

	a.loadSchedule(values)

	a.mu.Lock()
	defer a.mu.Unlock()

	cfg := tunnelSettingsOf(values)
	a.wantTun = cfg.complete()

	switch {
	case !cfg.complete():
		if a.tunnel != nil {
			a.log.Infof("wg: tunnel settings are incomplete, closing the tunnel")
			a.closeTunnelLocked()
		}
		a.tunnelErr = nil
	case a.tunnel != nil && cfg == a.tunnelCfg:
		// Unchanged. Re-opening would drop every transfer in flight for nothing.
	default:
		a.openTunnelLocked(ctx, cfg)
	}

	a.openRemoteLocked(values)
}

// openTunnelLocked replaces the running tunnel with one built from cfg.
func (a *App) openTunnelLocked(ctx context.Context, cfg tunnelSettings) {
	a.closeTunnelLocked()

	tun, err := wg.Open(cfg.wgConfig(func(f string, v ...any) { a.log.Debugf("wg: "+f, v...) }))
	if err != nil {
		a.tunnelErr = err
		a.log.Errorf("wg: %v", err)
		a.Event(ctx, store.LevelError, store.KindTunnelDown, "tunnel could not be opened: "+err.Error(), nil)
		return
	}

	a.tunnel, a.tunnelCfg, a.tunnelErr = tun, cfg, nil
	a.log.Infof("wg: up, %v -> %s, mtu %d", tun.Addrs(), tun.Endpoint(), wg.DefaultMTU)

	if a.handler != nil {
		a.serveTunnelLocked()
	}
}

// serveTunnelLocked puts the dashboard inside the tunnel. This is the property
// that costs nothing and replaces a port forward: the publisher reaches it over
// WireGuard with no configuration on the subscriber's side (DESIGN.md §2.2).
func (a *App) serveTunnelLocked() {
	ln, err := a.tunnel.Net().ListenTCP(&net.TCPAddr{Port: a.opts.TunnelPort})
	if err != nil {
		a.log.Errorf("dashboard: listen inside the tunnel: %v", err)
		return
	}

	srv := &http.Server{Handler: a.handler, ReadHeaderTimeout: 10 * time.Second}
	a.tunnelLn, a.tunnelSrv = ln, srv
	for _, addr := range a.tunnel.Addrs() {
		a.log.Infof("dashboard: http://%s:%d (inside the tunnel)", addr, a.opts.TunnelPort)
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Errorf("dashboard: tunnel listener: %v", err)
		}
	}()
}

func (a *App) closeTunnelLocked() {
	if a.tunnelSrv != nil {
		a.tunnelSrv.Close()
		a.tunnelSrv = nil
	}
	a.tunnelLn = nil
	if a.tunnel != nil {
		a.tunnel.Close()
		a.tunnel = nil
	}
	a.tunnelCfg = tunnelSettings{}
	a.up = false
	a.remote, a.remoteErr = nil, nil
}

// openRemoteLocked rebuilds the WebDAV client. It is rebuilt on every reload
// because it carries the tunnel's transport, which a reconnect replaces.
func (a *App) openRemoteLocked(values store.Values) {
	// The transport about to be replaced holds idle connections to the publisher;
	// without this they sit until IdleConnTimeout for nothing.
	if a.remoteTr != nil {
		a.remoteTr.CloseIdleConnections()
		a.remoteTr = nil
	}

	url := strings.TrimSpace(values.Get(store.KeyRemoteURL))
	if url == "" {
		a.remote, a.remoteErr = nil, nil
		return
	}

	cfg := webdav.Config{
		BaseURL:   url,
		Username:  values.Get(store.KeyRemoteUser),
		Password:  values.Get(store.KeyRemotePassword),
		UserAgent: "jcc-mirror/" + a.opts.Version,
	}
	// Always a transport of ours, tunnel or not: it is what counts the bytes the
	// Bandwidth view draws.
	a.remoteTr = a.meteredTransportLocked()
	cfg.Transport = a.remoteTr

	client, err := webdav.New(cfg)
	a.remote, a.remoteErr = client, err
	if err != nil {
		a.log.Errorf("webdav: %v", err)
	}
}

// Remote returns the configured WebDAV client, or an error explaining which half
// of the setup is still missing.
func (a *App) Remote() (*webdav.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch {
	case a.remoteErr != nil:
		return nil, a.remoteErr
	case a.remote == nil:
		return nil, errors.New("no remote configured: set the WebDAV base URL")
	case a.tunnel == nil && a.wantTun:
		// The request would go out of the host network instead, which for a private
		// WG address means a slow timeout rather than a clear answer. With no tunnel
		// configured at all it is let through: that is the local-fake case.
		return nil, errors.New("the tunnel is configured but not up, so the remote is unreachable")
	}
	return a.remote, nil
}

// PublicKey is the half of our WireGuard identity that has to be pasted into the
// rootserver's peer entry.
func (a *App) PublicKey(ctx context.Context) (string, error) {
	priv, err := a.store.ConfigGet(ctx, store.KeyWGPrivateKey)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(priv) == "" {
		return "", errors.New("no private key yet")
	}
	return wg.PublicKey(priv)
}

// Event appends to the event log. It takes no lock, so it is callable from the
// tunnel paths that already hold one. A failure to record is logged and
// swallowed: the event log is telemetry, and losing a line must never fail the
// operation that produced it.
func (a *App) Event(ctx context.Context, level, kind, message string, data map[string]any) {
	a.eventFor(ctx, nil, level, kind, message, data)
}

// eventFor is Event for something that belongs to one pair.
func (a *App) eventFor(ctx context.Context, pairID *int64, level, kind, message string, data map[string]any) {
	e := store.Event{Level: level, Kind: kind, PairID: pairID, Message: message, Data: data}
	if err := a.store.AppendEvent(ctx, &e); err != nil {
		a.log.Errorf("events: %v", err)
	}
}

// reconcileLoop watches the tunnel: it retries one that would not open, and
// reports the up/down edge once rather than every tick, which is the same
// discipline the notifications need later (DESIGN.md §4.1).
func (a *App) reconcileLoop(ctx context.Context) {
	tick := time.NewTicker(reconcileInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.reconcile(ctx)
		}
	}
}

func (a *App) reconcile(ctx context.Context) {
	a.mu.Lock()
	tun, tunErr := a.tunnel, a.tunnelErr
	wasUp := a.up
	a.mu.Unlock()

	if tun == nil {
		if tunErr == nil {
			// Unconfigured is not down: a first boot has no tunnel by definition,
			// and a condition left raised here would never clear.
			a.notifyTunnel(ctx, true, "no tunnel is configured")
			return
		}
		a.notifyTunnel(ctx, false, tunErr.Error())
		a.Reload(ctx) // the endpoint may resolve now, or the rootserver may be back
		return
	}

	up, detail := tunnelUp(tun)
	// The notification is raised every tick rather than only on the edge: what
	// decides it is how long the tunnel has been down, which changes while the
	// state does not.
	a.notifyTunnel(ctx, up, detail)
	if up == wasUp {
		return
	}

	a.mu.Lock()
	a.up = up
	a.mu.Unlock()

	if up {
		a.log.Infof("wg: %s", detail)
		a.Event(ctx, store.LevelInfo, store.KindTunnelUp, "tunnel up: "+detail, nil)
	} else {
		a.log.Warnf("wg: %s", detail)
		a.Event(ctx, store.LevelWarn, store.KindTunnelDown, "tunnel down: "+detail, nil)
	}
}

// notifyTunnel raises the tunnel-down condition once it has been down longer than
// the grace period. A rootserver rebooting should not wake anyone, and a link
// that has been down since Tuesday should not say so every half minute
// (DESIGN.md §4.1).
func (a *App) notifyTunnel(ctx context.Context, up bool, detail string) {
	a.mu.Lock()
	switch {
	case up:
		a.downSince = time.Time{}
	case a.downSince.IsZero():
		a.downSince = time.Now()
	}
	since := a.downSince
	a.mu.Unlock()

	grace := 15 * time.Minute
	if values, err := a.store.Config(ctx); err == nil {
		grace = values.Duration(store.KeyNotifyTunnelGrace)
	}

	down := !up && time.Since(since) >= grace
	onset := ""
	if down {
		onset = "The tunnel has been down for " + format.Duration(time.Since(since)) + " — " + detail
	}
	a.NotifyEdge(ctx, store.NotifyTunnelDown, 0, down, onset, "The tunnel is back")
}

// tunnelUp reports whether any peer has handshaked recently enough to call the
// link live.
func tunnelUp(tun *wg.Tunnel) (bool, string) {
	st, err := tun.Status()
	if err != nil {
		return false, "device status unavailable: " + err.Error()
	}
	for _, p := range st.Peers {
		age, ok := p.HandshakeAge()
		if !ok {
			continue
		}
		if age < handshakeStale {
			return true, fmt.Sprintf("handshake with %s %s ago", p.Endpoint, age.Round(time.Second))
		}
	}
	return false, "no handshake in the last " + handshakeStale.String()
}

// tunnelSettings is the comparable half of the tunnel configuration - wg.Config
// carries a log function and so cannot be compared. A reload that finds nothing
// changed leaves the device alone, rather than dropping every transfer in flight
// because an unrelated setting was saved.
type tunnelSettings struct {
	privateKey   string
	peerKey      string
	presharedKey string
	endpoint     string
	addresses    string
	allowedIPs   string
	dns          string
}

func tunnelSettingsOf(v store.Values) tunnelSettings {
	return tunnelSettings{
		privateKey:   strings.TrimSpace(v.Get(store.KeyWGPrivateKey)),
		peerKey:      strings.TrimSpace(v.Get(store.KeyWGPeerKey)),
		presharedKey: strings.TrimSpace(v.Get(store.KeyWGPresharedKey)),
		endpoint:     strings.TrimSpace(v.Get(store.KeyWGEndpoint)),
		addresses:    strings.TrimSpace(v.Get(store.KeyWGAddress)),
		allowedIPs:   strings.TrimSpace(v.Get(store.KeyWGAllowedIPs)),
		dns:          strings.TrimSpace(v.Get(store.KeyWGDNS)),
	}
}

// complete reports whether the tunnel can be opened at all. Everything blank is
// the first-boot state the Config view exists for, not an error.
func (t tunnelSettings) complete() bool {
	return t.privateKey != "" && t.peerKey != "" && t.endpoint != "" &&
		t.addresses != "" && t.allowedIPs != ""
}

func (t tunnelSettings) wgConfig(logf func(string, ...any)) wg.Config {
	return wg.Config{
		PrivateKey:    t.privateKey,
		PeerPublicKey: t.peerKey,
		PresharedKey:  t.presharedKey,
		Endpoint:      t.endpoint,
		Addresses:     t.addresses,
		AllowedIPs:    t.allowedIPs,
		DNS:           t.dns,
		Keepalive:     wg.DefaultKeepalive,
		MTU:           wg.DefaultMTU,
		Logf:          logf,
	}
}

// generateSetting produces the values that are never typed by hand.
func generateSetting(key string) (string, error) {
	switch key {
	case store.KeyWGPrivateKey:
		return wg.GenerateKey()
	case store.KeyDashboardToken:
		return newToken()
	default:
		return "", fmt.Errorf("no generator for %q", key)
	}
}
