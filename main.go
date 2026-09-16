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
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
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
	"blackforestbytes.com/jcc-mirror/smb"
	"blackforestbytes.com/jcc-mirror/store"
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
	{"supervise", groupDaemon, "run the daemon under the supervisor that undoes a bad self-update", cmdSupervise},
	{"update", groupDaemon, "check the share for a newer binary, install it, or step back off one", cmdUpdate},
	{"version", groupDaemon, "print the version and build timestamp", cmdVersion},

	{"remotes", groupMirror, "list, add, change and remove the publisher's shares the pairs read from", cmdRemotes},
	{"pairs", groupMirror, "list, add, change and remove the directory pairs", cmdPairs},
	{"scan", groupMirror, "walk a pair's remote root into the manifest", cmdScan},
	{"plan", groupMirror, "say what a sync would do, and change nothing", cmdPlan},
	{"sync", groupMirror, "queue the plan and transfer it", cmdSync},
	{"adopt", groupMirror, "recognise a USB bootstrap already on disk, matched on size", cmdAdopt},
	{"delete", groupMirror, "delete what the publisher no longer has; a guarded pair quarantines it, behind the guards", cmdDelete},
	{"trash", groupMirror, "list, restore from and empty the quarantine", cmdTrash},
	{"db", groupMirror, "the jCC database: its lock gate, the copies kept, and the way back", cmdDB},
	{"jobs", groupMirror, "inspect the transfer queue, including what failed and why", cmdJobs},
	{"schedule", groupMirror, "show or set the 7x24 transfer and scan windows", cmdSchedule},

	{"pubkey", groupDiagnostics, "derive the public key of -wg-key, for the rootserver peer entry", cmdPubkey},
	{"ping", groupDiagnostics, "ICMP-ping a WireGuard address through the tunnel (M0 step 1)", cmdPing},
	{"status", groupDiagnostics, "bring the tunnel up and report handshake, endpoint and counters (M0 step 2)", cmdStatus},
	{"ls", groupDiagnostics, "list one remote directory (M0 step 3)", cmdLs},
	{"walk", groupDiagnostics, "time a full metadata walk of the remote tree (M0 step 4)", cmdWalk},
	{"get", groupDiagnostics, "read a range of one file, optionally in parallel chunks (M0 step 5)", cmdGet},
	{"resume", groupDiagnostics, "abort a read mid-file and prove the resume is byte-identical (M0 step 5)", cmdResume},
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
				fmt.Fprintf(os.Stderr, "  %-10s %s\n", c.name, c.brief)
			}
		}
	}
	fmt.Fprintf(os.Stderr, "\nEvery command takes -h. The daemon is configured in the dashboard; the mirror\nand the diagnostics take -data to work on the same sqlite state, -from to read\na stored remote other than the pair's own or the first, and -remote-dir to run\nagainst a local directory instead of any share.\n")
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

	smbHost   string
	smbShare  string
	smbPath   string
	smbUser   string
	smbPass   string
	smbDomain string
	from      string
	remoteDir string

	dataDir string
	verbose bool

	// limit is registered by sync alone, since it is the only command that moves
	// bytes, but it is read where the engine is built.
	limit string

	// fs is kept so fromStore can tell a flag that was given from one that only
	// carries its default. An int flag has no empty value to test for, and 0 is a
	// legal keepalive, so there is no sentinel that would do instead.
	fs *flag.FlagSet

	stored     store.Values
	storedOnce sync.Once
	storedErr  error

	// fromRemote is what -from resolves to, or the first remote when it was not
	// given. The error is kept rather than returned, because only the commands
	// that open the share have any use for a remote.
	fromRemote    store.Remote
	fromRemoteErr error

	// remoteName is the stored remote the SMB settings were completed from. Once
	// it is set - or remotePicked is, for a remote the flags describe alone - no
	// other remote is mixed in.
	remoteName   string
	remotePicked bool
}

// newFlagSet builds a diagnostic command's flag set with the shared flags already
// registered. Nothing is read from the environment: a setting comes from a flag,
// or - with -data - from the same sqlite config the daemon runs on (DESIGN.md §6).
func newFlagSet(name string) (*flag.FlagSet, *config) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	cfg := &config{fs: fs}

	fs.StringVar(&cfg.wgPrivateKey, "wg-key", "", "our WireGuard private key, base64")
	fs.StringVar(&cfg.wgPeerKey, "wg-peer", "", "the rootserver's WireGuard public key, base64")
	fs.StringVar(&cfg.wgPresharedKey, "wg-psk", "", "optional preshared key, base64")
	fs.StringVar(&cfg.wgEndpoint, "wg-endpoint", "", "rootserver host:port")
	fs.StringVar(&cfg.wgAddress, "wg-address", "", "our address inside the tunnel, e.g. 10.0.0.3/32")
	fs.StringVar(&cfg.wgAllowedIPs, "wg-allowed-ips", "", "CIDRs routed into the tunnel; must cover the whole WG subnet, not just the publisher")
	fs.StringVar(&cfg.wgDNS, "wg-dns", "", "resolvers reachable through the tunnel, only needed when -host is a name")
	fs.IntVar(&cfg.wgKeepalive, "wg-keepalive", wg.DefaultKeepalive, "persistent keepalive in seconds")
	fs.IntVar(&cfg.wgMTU, "wg-mtu", wg.DefaultMTU, "tunnel MTU; 1420 unless you know otherwise")
	fs.BoolVar(&cfg.wgVerbose, "wg-verbose", false, "log the wireguard-go device chatter, including handshakes")
	fs.BoolVar(&cfg.noTunnel, "no-tunnel", false, "talk to -host directly, without WireGuard - for testing against a local server")

	fs.StringVar(&cfg.smbHost, "host", "", "the publisher's SMB server, host or host:port")
	fs.StringVar(&cfg.smbShare, "share", "", "the share to mount, named the way the server names it")
	fs.StringVar(&cfg.smbPath, "share-path", "", "directory inside the share that is the remote root; empty is the share itself")
	fs.StringVar(&cfg.smbUser, "user", "", "SMB user")
	fs.StringVar(&cfg.smbPass, "pass", "", "SMB password")
	fs.StringVar(&cfg.smbDomain, "domain", "", "optional NTLM domain or workgroup")
	fs.StringVar(&cfg.from, "from", "", "the stored remote, by name or id; the SMB flags left unset are taken from it. Defaults to a pair's own remote, and elsewhere to the first one")
	fs.StringVar(&cfg.remoteDir, "remote-dir", "", "serve this local directory as the remote instead of the share, for development and testing")

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
		if cfg.stored, cfg.storedErr = st.Config(ctx); cfg.storedErr != nil {
			return
		}
		cfg.fromRemote, cfg.fromRemoteErr = st.ResolveRemote(ctx, cfg.from)
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
	} {
		if *f.dst == "" {
			*f.dst = cfg.stored.Get(f.key)
		}
	}

	given := map[string]bool{}
	cfg.fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	for _, f := range []struct {
		dst  *int
		flag string
		key  string
	}{
		{&cfg.wgMTU, "wg-mtu", store.KeyWGMTU},
		{&cfg.wgKeepalive, "wg-keepalive", store.KeyWGKeepalive},
	} {
		// Presence rather than a non-zero value: 0 is a keepalive that means "send
		// none", and reading it as "nothing stored" would put 25 back.
		if !given[f.flag] && cfg.stored.Get(f.key) != "" {
			*f.dst = cfg.stored.Int(f.key)
		}
	}
	return nil
}

// remoteFromStore fills the SMB settings left unset from a stored remote: the
// one -from names, or the first. A mirror command has picked its pair's remote
// before this runs, and that choice stands.
func (cfg *config) remoteFromStore(ctx context.Context) error {
	if err := cfg.fromStore(ctx); err != nil {
		return err
	}
	if cfg.remotePicked {
		return nil
	}
	if cfg.dataDir == "" {
		if strings.TrimSpace(cfg.from) != "" {
			return fmt.Errorf("-from names a stored remote, so it needs -data")
		}
		return nil
	}

	switch {
	case cfg.fromRemoteErr == nil:
		cfg.useRemote(cfg.fromRemote)
	case errors.Is(cfg.fromRemoteErr, store.ErrNoRemote) && strings.TrimSpace(cfg.from) == "":
		// Nothing is stored yet, so the flags are all there is.
	default:
		return fmt.Errorf("-from: %w", cfg.fromRemoteErr)
	}
	return nil
}

// useRemote takes the SMB settings no flag gave from r, and makes r the remote
// for the rest of the command.
func (cfg *config) useRemote(r store.Remote) {
	for _, f := range []struct {
		dst *string
		val string
	}{
		{&cfg.smbHost, r.Host},
		{&cfg.smbShare, r.Share},
		{&cfg.smbPath, r.Path},
		{&cfg.smbUser, r.User},
		{&cfg.smbPass, r.Password},
		{&cfg.smbDomain, r.Domain},
	} {
		if *f.dst == "" {
			*f.dst = f.val
		}
	}
	cfg.remoteName = r.Name
	cfg.remotePicked = true
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

// openSMB builds the SMB client, dialing through tun when there is one.
func (cfg *config) openSMB(ctx context.Context, tun *wg.Tunnel) (*smb.Client, error) {
	if err := cfg.remoteFromStore(ctx); err != nil {
		return nil, err
	}
	if cfg.smbHost == "" || cfg.smbShare == "" {
		return nil, fmt.Errorf("missing -host or -share, and no stored remote to take them from (see -h, and `jcc-mirror remotes add -h`)")
	}

	scfg := smb.Config{
		Host:     cfg.smbHost,
		Share:    cfg.smbShare,
		Path:     cfg.smbPath,
		User:     cfg.smbUser,
		Password: cfg.smbPass,
		Domain:   cfg.smbDomain,
	}
	if tun != nil {
		scfg.Dial = tun.DialContext
	}
	return smb.New(scfg)
}

// openBoth is the setup every remote command shares. The returned close function
// is safe to defer even when the tunnel was never opened, and it ends the SMB
// session rather than leaving the server holding one.
func (cfg *config) openBoth(ctx context.Context, logger *logs.Logger) (*smb.Client, *wg.Tunnel, func(), error) {
	tun, err := cfg.openTunnel(ctx, logger)
	if err != nil {
		return nil, nil, func() {}, err
	}
	closeTun := func() {
		if tun != nil {
			tun.Close()
		}
	}

	client, err := cfg.openSMB(ctx, tun)
	if err != nil {
		closeTun()
		return nil, nil, func() {}, err
	}
	if cfg.remoteName != "" {
		logger.Infof("smb: %s, remote %q", client.Target(), cfg.remoteName)
	}
	return client, tun, func() {
		client.Close()
		closeTun()
	}, nil
}

// openEngineRemote is what the mirror commands run against. With -remote-dir a
// local directory stands in for the publisher's share, which is what lets the
// whole of M2 be exercised with no NAS, no tunnel and no share to mount; it wins
// over -host, -from and a pair's remote, and nothing about the tunnel is touched.
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
