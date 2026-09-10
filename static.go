package main

// Static settings (DESIGN.md §6). These are compiled in because they do not vary
// between installs: there is no bootstrap layer, no config file is mounted and
// nothing is read from the environment. Everything that does vary lives in sqlite
// and is edited in the dashboard.
const (
	// staticDataDir is the volume mount: sqlite, the WireGuard identity, the
	// jClipCorn database backups and, from M7, the binaries.
	staticDataDir = "/data"

	// staticLANListen is the dashboard on the host network. Inside the container
	// there is no LAN address to bind to, so what actually scopes this is the
	// compose port mapping: the dashboard is never published beyond the LAN.
	staticLANListen = ":8080"

	// staticTunnelPort is the dashboard inside the tunnel. Listening there is what
	// lets the publisher reach it over WireGuard with no port forward and nothing
	// configured on the subscriber's router (DESIGN.md §2.2).
	staticTunnelPort = 8080
)

// Build identity, set by the Makefile via -ldflags. buildStamp is what the
// self-updater compares against the binary's Last-Modified in M7.
var (
	version    = "dev"
	buildStamp = "unknown"
)
