// jcc-mirror replicates a jClipCorn collection one way, from the publisher's NAS
// to the subscriber's Synology, over a WireGuard tunnel that exists only inside
// this process (DESIGN.md).
//
// `serve` is the program: it is what the container runs and what the dashboard
// configures. The remaining commands are the M0 spike, kept because they are the
// diagnostics - each one answers a question the design rests on, and they run in
// the order `jcc-mirror help` prints them.
//
// Nothing here writes to the publisher, and nothing here runs on the publisher.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	// The timezone the schedule is read in is a setting of its own, so the zone
	// database has to be in the binary: a distroless image or a bare NAS may not
	// carry one, and a grid that silently fell back to UTC would run at the wrong
	// hours twice a year (DESIGN.md §6).
	_ "time/tzdata"

	"blackforestbytes.com/jcc-mirror/logs"
	"blackforestbytes.com/jcc-mirror/remote"
	"blackforestbytes.com/jcc-mirror/remote/localfs"
	"blackforestbytes.com/jcc-mirror/store"
	"blackforestbytes.com/jcc-mirror/webdav"
	"blackforestbytes.com/jcc-mirror/wg"
)

type command struct {
	name  string
	group int
	brief string
	run   func(ctx context.Context, args []string) error
}

// The sections of the usage text, in the order it prints them.
const (
	groupDaemon = iota
	groupMirror
	groupDiagnostics
)

var groups = []struct {
	id    int
	title string
}{
	{groupDaemon, "the daemon"},
	{groupMirror, "the mirror"},
	{groupDiagnostics, "diagnostics, in the order they are meant to be run"},
}

var commands = []command{
	{"serve", groupDaemon, "run the daemon: sqlite state, tunnel, dashboard", cmdServe},
	{"version", groupDaemon, "print the version and build timestamp", cmdVersion},

	{"pairs", groupMirror, "list, add, change and remove the directory pairs", cmdPairs},
	{"scan", groupMirror, "walk a pair's remote root into the manifest", cmdScan},
	{"plan", groupMirror, "say what a sync would do, and change nothing", cmdPlan},
	{"sync", groupMirror, "queue the plan and transfer it", cmdSync},
	{"adopt", groupMirror, "recognise a USB bootstrap already on disk, matched on size", cmdAdopt},
	{"delete", groupMirror, "quarantine what the publisher no longer has, behind the guards", cmdDelete},
	{"trash", groupMirror, "list, restore from and empty the quarantine", cmdTrash},
	{"jobs", groupMirror, "inspect the transfer queue, including what failed and why", cmdJobs},
	{"schedule", groupMirror, "show or set the 7x24 transfer and scan windows", cmdSchedule},

	{"pubkey", groupDiagnostics, "derive the public key of -wg-key, for the rootserver peer entry", cmdPubkey},
	{"ping", groupDiagnostics, "ICMP-ping a WireGuard address through the tunnel (M0 step 1)", cmdPing},
	{"status", groupDiagnostics, "bring the tunnel up and report handshake, endpoint and counters (M0 step 2)", cmdStatus},
	{"propfind", groupDiagnostics, "list one remote directory (M0 step 3)", cmdPropfind},
	{"depth", groupDiagnostics, "check whether the server honours Depth: infinity (M0 step 3)", cmdDepth},
	{"walk", groupDiagnostics, "time a full metadata walk of the remote tree (M0 step 4)", cmdWalk},
	{"get", groupDiagnostics, "ranged GET, optionally in parallel chunks (M0 step 5)", cmdGet},
	{"resume", groupDiagnostics, "abort a GET mid-file and prove the resume is byte-identical (M0 step 5)", cmdResume},
	{"soak", groupDiagnostics, "stream for hours and report stalls, errors and throughput (M0 step 5)", cmdSoak},
}

func main() {
	log.SetFlags(log.Ltime)
	if err := run(); err != nil {
		log.Printf("[fatal] %v", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		return fmt.Errorf("no command given")
	}

	name := os.Args[1]
	switch name {
	case "help", "-h", "--help":
		usage()
		return nil
	case "-version", "--version":
		name = "version"
	}

	for _, c := range commands {
		if c.name != name {
			continue
		}
		// Ctrl-C and docker stop end the context rather than the process, so a soak
		// or a walk still prints its summary.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return c.run(ctx, os.Args[2:])
	}

	usage()
	return fmt.Errorf("unknown command %q", name)
}

func usage() {
	fmt.Fprintf(os.Stderr, "jcc-mirror %s\n\nusage: jcc-mirror <command> [flags]\n", version)
	for _, g := range groups {
		fmt.Fprintf(os.Stderr, "\n%s:\n\n", g.title)
		for _, c := range commands {
			if c.group == g.id {
				fmt.Fprintf(os.Stderr, "  %-9s %s\n", c.name, c.brief)
			}
		}
	}
	fmt.Fprintf(os.Stderr, "\nEvery command takes -h. The daemon is configured in the dashboard; the mirror\nand the diagnostics take -data to work on the same sqlite state, and -remote-dir\nto run against a local directory instead of the publisher's share.\n")
}

func cmdVersion(context.Context, []string) error {
	fmt.Printf("jcc-mirror %s (built %s)\n", version, buildStamp)
	return nil
}

// config holds the settings every command shares, filled from the common flags.
type config struct {
	wgPrivateKey   string
	wgPeerKey      string
	wgPresharedKey string
	wgEndpoint     string
	wgAddress      string
	wgAllowedIPs   string
	wgDNS          string
	wgKeepalive    int
	wgMTU          int
	wgVerbose      bool
	noTunnel       bool

	davURL    string
	davUser   string
	davPass   string
	remoteDir string

	dataDir string
	verbose bool

	// limit is registered by sync alone, since it is the only command that moves
	// bytes, but it is read where the engine is built.
	limit string

	stored     store.Values
	storedOnce sync.Once
	storedErr  error
}

// newFlagSet builds a diagnostic command's flag set with the shared flags already
// registered. Nothing is read from the environment: a setting comes from a flag,
// or - with -data - from the same sqlite config the daemon runs on (DESIGN.md §6).
func newFlagSet(name string) (*flag.FlagSet, *config) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	cfg := &config{}

	fs.StringVar(&cfg.wgPrivateKey, "wg-key", "", "our WireGuard private key, base64")
	fs.StringVar(&cfg.wgPeerKey, "wg-peer", "", "the rootserver's WireGuard public key, base64")
	fs.StringVar(&cfg.wgPresharedKey, "wg-psk", "", "optional preshared key, base64")
	fs.StringVar(&cfg.wgEndpoint, "wg-endpoint", "", "rootserver host:port")
	fs.StringVar(&cfg.wgAddress, "wg-address", "", "our address inside the tunnel, e.g. 10.0.0.3/32")
	fs.StringVar(&cfg.wgAllowedIPs, "wg-allowed-ips", "", "CIDRs routed into the tunnel; must cover the whole WG subnet, not just the publisher")
	fs.StringVar(&cfg.wgDNS, "wg-dns", "", "resolvers reachable through the tunnel, only needed for a hostname in -url")
	fs.IntVar(&cfg.wgKeepalive, "wg-keepalive", wg.DefaultKeepalive, "persistent keepalive in seconds")
	fs.IntVar(&cfg.wgMTU, "wg-mtu", wg.DefaultMTU, "tunnel MTU; 1420 unless you know otherwise")
	fs.BoolVar(&cfg.wgVerbose, "wg-verbose", false, "log the wireguard-go device chatter, including handshakes")
	fs.BoolVar(&cfg.noTunnel, "no-tunnel", false, "talk to -url directly, without WireGuard - for testing against a local server")

	fs.StringVar(&cfg.davURL, "url", "", "WebDAV base URL, the remote root of the mirror")
	fs.StringVar(&cfg.davUser, "user", "", "WebDAV user")
	fs.StringVar(&cfg.davPass, "pass", "", "WebDAV password")
	fs.StringVar(&cfg.remoteDir, "remote-dir", "", "serve this local directory as the remote instead of WebDAV, for development and testing")

	fs.StringVar(&cfg.dataDir, "data", "", "take the settings left unset from the store in this data directory, e.g. "+staticDataDir)
	fs.BoolVar(&cfg.verbose, "v", false, "verbose output")

	return fs, cfg
}

// fromStore fills the settings left unset from the daemon's sqlite config. A flag
// always wins: trying something other than the stored value is the whole point of
// the diagnostic commands.
func (cfg *config) fromStore(ctx context.Context) error {
	if cfg.dataDir == "" {
		return nil
	}
	cfg.storedOnce.Do(func() {
		st, err := store.Open(ctx, cfg.dataDir)
		if err != nil {
			cfg.storedErr = err
			return
		}
		defer st.Close()
		cfg.stored, cfg.storedErr = st.Config(ctx)
	})
	if cfg.storedErr != nil {
		return cfg.storedErr
	}

	for _, f := range []struct {
		dst *string
		key string
	}{
		{&cfg.wgPrivateKey, store.KeyWGPrivateKey},
		{&cfg.wgPeerKey, store.KeyWGPeerKey},
		{&cfg.wgPresharedKey, store.KeyWGPresharedKey},
		{&cfg.wgEndpoint, store.KeyWGEndpoint},
		{&cfg.wgAddress, store.KeyWGAddress},
		{&cfg.wgAllowedIPs, store.KeyWGAllowedIPs},
		{&cfg.wgDNS, store.KeyWGDNS},
		{&cfg.davURL, store.KeyRemoteURL},
		{&cfg.davUser, store.KeyRemoteUser},
		{&cfg.davPass, store.KeyRemotePassword},
	} {
		if *f.dst == "" {
			*f.dst = cfg.stored.Get(f.key)
		}
	}
	return nil
}

// openTunnel brings the tunnel up, or returns nil when -no-tunnel was given.
func (cfg *config) openTunnel(ctx context.Context, logger *logs.Logger) (*wg.Tunnel, error) {
	if err := cfg.fromStore(ctx); err != nil {
		return nil, err
	}
	if cfg.noTunnel {
		logger.Infof("wg: -no-tunnel, going straight out of the host network")
		return nil, nil
	}
	if cfg.wgPrivateKey == "" || cfg.wgPeerKey == "" || cfg.wgEndpoint == "" || cfg.wgAddress == "" || cfg.wgAllowedIPs == "" {
		return nil, fmt.Errorf("missing tunnel config: -wg-key, -wg-peer, -wg-endpoint, -wg-address and -wg-allowed-ips are all required (see -h)")
	}

	devLog := func(string, ...any) {}
	if cfg.wgVerbose {
		devLog = func(f string, a ...any) { logger.Debugf("wg: "+f, a...) }
	}

	tun, err := wg.Open(wg.Config{
		PrivateKey:    cfg.wgPrivateKey,
		PeerPublicKey: cfg.wgPeerKey,
		PresharedKey:  cfg.wgPresharedKey,
		Endpoint:      cfg.wgEndpoint,
		Addresses:     cfg.wgAddress,
		AllowedIPs:    cfg.wgAllowedIPs,
		DNS:           cfg.wgDNS,
		Keepalive:     cfg.wgKeepalive,
		MTU:           cfg.wgMTU,
		Logf:          devLog,
	})
	if err != nil {
		return nil, err
	}

	logger.Infof("wg: up, %v -> %s, mtu %d", tun.Addrs(), tun.Endpoint(), cfg.wgMTU)
	return tun, nil
}

// openDAV builds the WebDAV client, routed through tun when there is one.
func (cfg *config) openDAV(ctx context.Context, tun *wg.Tunnel) (*webdav.Client, error) {
	if err := cfg.fromStore(ctx); err != nil {
		return nil, err
	}
	if cfg.davURL == "" {
		return nil, fmt.Errorf("missing -url (see -h)")
	}

	dav := webdav.Config{
		BaseURL:  cfg.davURL,
		Username: cfg.davUser,
		Password: cfg.davPass,
	}
	if tun != nil {
		dav.Transport = tun.Transport()
	}
	return webdav.New(dav)
}

// openBoth is the setup every WebDAV command shares. The returned close function
// is safe to defer even when the tunnel was never opened.
func (cfg *config) openBoth(ctx context.Context, logger *logs.Logger) (*webdav.Client, *wg.Tunnel, func(), error) {
	tun, err := cfg.openTunnel(ctx, logger)
	if err != nil {
		return nil, nil, func() {}, err
	}
	closeFn := func() {
		if tun != nil {
			tun.Close()
		}
	}

	client, err := cfg.openDAV(ctx, tun)
	if err != nil {
		closeFn()
		return nil, nil, func() {}, err
	}
	return client, tun, closeFn, nil
}

// openEngineRemote is what the mirror commands run against. With -remote-dir a
// local directory stands in for the publisher's share, which is what lets the
// whole of M2 be exercised with no NAS, no tunnel and no WebDAV server; it wins
// over -url, and nothing about the tunnel is touched.
func (cfg *config) openEngineRemote(ctx context.Context, logger *logs.Logger) (remote.Remote, func(), error) {
	if cfg.remoteDir != "" {
		fs, err := localfs.New(cfg.remoteDir)
		if err != nil {
			return nil, func() {}, err
		}
		logger.Infof("remote: %s stands in for the publisher's share", fs.Root())
		return fs, func() {}, nil
	}

	client, _, closeFn, err := cfg.openBoth(ctx, logger)
	if err != nil {
		return nil, func() {}, err
	}
	return client, closeFn, nil
}
