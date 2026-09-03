# jcc-mirror

One-way replication of a jClipCorn collection from the publisher's NAS to the
subscriber's Synology. See `DESIGN.md` for the design; this README covers what is
built so far.

**Status: M0.** The spike, and nothing else. It answers the questions that could
invalidate the design, and deliberately transfers nothing permanently: it walks,
measures, and writes only where you point `-out`.

## What M0 has to answer

| # | Question | Command | Why it could sink the design |
|---|---|---|---|
| 1 | Does the rootserver forward between its spokes? | `ping` | Both NASes are clients of the same WireGuard server. A server set up as an internet gateway will not route between them, and nothing else here can work until it does. |
| 2 | Does the tunnel come up from inside an unprivileged container? | `status` | The whole no-`NET_ADMIN` deployment rests on wireguard-go plus netstack behaving on the Synology. |
| 3 | Does DSM's WebDAV answer PROPFIND the way we assume? | `propfind`, `depth` | Href shape, namespace prefixes, and whether `Depth: infinity` is honoured. The last one is thousands of round trips per scan. |
| 4 | How long does a full metadata walk take? | `walk` | There is no remote manifest, so this is what every scan costs, forever. It sets the scan schedule. |
| 5 | Does DSM hold up under a long ranged GET? | `get`, `resume`, `soak` | Resume is the property the transfer engine is built on. If ranged GET is unreliable, a 40 GB file interrupted at 39 GB starts again from zero. |
| 6 | Can the publisher reach a listener on the tunnel? | `serve` | This is what makes the dashboard free - no port forward, no host-side WireGuard, nothing on the subscriber's router. |

## Running it, in order

Nothing is read from the environment and no addresses are baked into the binary,
so the tunnel settings are given as flags. Two shell variables keep the examples
short:

```bash
make build

wg genkey > wg.key
WG="-wg-key $(cat wg.key) -wg-peer <rootserver-pubkey> -wg-endpoint <host:port>"
WG="$WG -wg-address <our-tunnel-addr>/32 -wg-allowed-ips <wg-subnet>/24"
DAV="-url http://<publisher-tunnel-addr>:5005/media -user <user> -pass <password>"

# 0. What to paste into the rootserver's peer entry for this container.
./jcc-mirror pubkey $WG

# 1. The route between spokes. Do this first; everything else assumes it.
./jcc-mirror ping $WG -target <publisher-tunnel-addr> -count 10

# 2. The tunnel from where it will actually run.
./jcc-mirror status $WG $DAV

# 3. What DSM's WebDAV actually returns. -raw prints the XML, which is where the
#    two likely surprises (href shape, D:/d: prefixes) are visible.
./jcc-mirror propfind $WG $DAV -path "" -raw
./jcc-mirror depth $WG $DAV -path "ClipCornDB"      # a SMALL subdirectory, see below

# 4. The measurement the scan schedule is built on. Expect minutes, not seconds.
./jcc-mirror walk $WG $DAV -workers 8 -out /data/manifest.ndjson

# 5. Transfers. resume is the one that matters most.
./jcc-mirror get    $WG $DAV -path "Filme/Some.Big.File.mkv" -length $((2<<30)) -chunks 1
./jcc-mirror get    $WG $DAV -path "Filme/Some.Big.File.mkv" -length $((2<<30)) -chunks 4
./jcc-mirror resume $WG $DAV -path "Filme/Some.Big.File.mkv" -length $((256<<20))
./jcc-mirror soak   $WG $DAV -path "Filme/Some.Big.File.mkv" -duration 4h

# 6. Then ask the publisher to open http://<our-wg-address>:8080 in a browser.
./jcc-mirror serve $WG
```

Every command takes `-h`, and every one of them can be pointed at a local WebDAV
server instead of the tunnel with `-no-tunnel`.

## Reading the results

- **`ping` gets no replies but the handshake succeeds.** The tunnel is fine and
  the spoke-to-spoke route is not. Check `net.ipv4.ip_forward=1` on the
  rootserver and that its peer entries' `AllowedIPs` cover *both* clients. This
  is not a jcc-mirror bug and it is the most likely way M0 fails.
- **`depth` says infinity is refused.** Expected, and fine - RFC 4918 allows it.
  The scanner then walks one PROPFIND per directory, which is what the design
  assumes anyway. Point the probe at a *small* subdirectory: if the server does
  honour it, an infinity request on the collection root returns the entire tree
  as a single XML document.
- **`walk` reports non-NFC names.** Part of the tree passed through macOS. The
  diff must normalize, or every title with an umlaut looks new on every scan.
- **`resume` reports differing hashes.** Ranged resume is not usable against this
  server, and the transfer design needs rethinking before M2. This is the single
  most consequential result in M0.
- **`soak` reports a first error at a suspiciously round time.** DSM drops long
  transfers on a timer. Not fatal - the job state machine has to treat a dropped
  connection as routine - but it decides how aggressive the retry policy is.

## Configuration

There is no config file, no `.env`, and nothing is read from the environment. The
spike takes every setting as a flag; from M1 on they live in sqlite under `/data`
and are entered in the dashboard (`DESIGN.md` §6). Only true invariants — MTU,
keepalive, listen ports, the data directory — are compiled in.

| Flag | Meaning |
|---|---|
| `-wg-key` | Our WireGuard private key, base64. `wg genkey` |
| `-wg-peer` | The rootserver's public key, base64 |
| `-wg-endpoint` | `host:port` of the rootserver. A hostname is resolved once at startup and never again |
| `-wg-address` | Our address inside the tunnel, e.g. `10.13.13.3/32` |
| `-wg-allowed-ips` | CIDRs routed into the tunnel. Must cover the whole WG subnet, not just the publisher |
| `-wg-psk` | Optional preshared key |
| `-wg-keepalive` | Seconds; 25 by default, because both ends are behind NAT |
| `-wg-mtu` | 1420 unless you know otherwise |
| `-wg-dns` | Only needed when `-url` uses a hostname |
| `-target` | The publisher's address inside the tunnel (`ping` only) |
| `-url` | WebDAV base URL. Its path is the remote root: every path in every command is relative to it |
| `-user` | WebDAV user, read-only |
| `-pass` | WebDAV password |

## Publisher-side setup

Nothing of ours runs there. Enable the stock DSM **WebDAV Server** package,
share the directories, and create a read-only account. HTTP on 5005 is enough:
the tunnel already encrypts and authenticates every packet, and plain HTTP is
faster through netstack.

## Development

```bash
make test        # unit tests plus an end-to-end run against a local WebDAV server
make vet
make build
```

The WebDAV client and the walk are tested against `golang.org/x/net/webdav`
serving a temporary directory, so the whole engine runs with no network and no
NAS. That is the reason `remote.Remote` is an interface.

## Structure

```
main.go        command dispatch, shared config and flags
cmd_*.go       one file per M0 command
wg/            wireguard-go + netstack: the tunnel, ping and device status
webdav/        PROPFIND, HEAD and ranged GET; propfind.go is the pure decoder
remote/        the three-method interface the rest of the engine will use
deploy/        Dockerfile and compose for running it on the Synology
```
