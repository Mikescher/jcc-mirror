# jcc-mirror

One-way replication of a jClipCorn collection from the publisher's NAS to the
subscriber's Synology. See `DESIGN.md` for the design; this README covers what is
built so far.

**Status: M1.** The skeleton. The binary is a daemon now: sqlite state, a config
table with an audit trail, a setup view, the `Remote` interface with a WebDAV
implementation and a local fake, an event log and `/healthz`. It configures
itself and reports on itself — it does not yet scan, diff or transfer anything.
That is M2.

## Running it

```bash
cd deploy && docker compose up -d && docker compose logs -f
```

Nothing has to be known before the process starts. The LAN dashboard does not
depend on the tunnel, so a first boot against an empty database answers on
`:8080` with the setup view:

1. The container log prints the **dashboard token** and this container's
   **WireGuard public key**. Both are generated on first start; the private half
   of the key never leaves the data volume.
2. Paste that public key into the rootserver's peer entry.
3. Open `http://<synology>:8080`, unlock with the token, and fill in the
   rootserver's public key, its endpoint, this container's tunnel address, the
   allowed IPs, and the WebDAV URL and credentials.
4. Save. The tunnel comes up in place — no restart — and the same dashboard also
   starts answering on `http://<our-tunnel-address>:8080`, which is how the
   publisher reaches it with no port forward.
5. **List the remote root** proves the whole path end to end. Its answer lands in
   the event log.

Reading the dashboard needs no token. Changing anything does, including from
`curl`:

```bash
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{"remote.url":"http://10.13.13.2:5005/media"}' \
     http://<synology>:8080/api/config
```

| Endpoint | |
|---|---|
| `GET /` | Setup view |
| `GET /healthz` | Status as JSON; 503 only when the database has stopped answering |
| `GET /api/status` · `/api/config` · `/api/config/audit` · `/api/events` | Read views. Secrets are never returned |
| `POST /api/config` · `/api/remote/probe` · `/api/login` | Token required |

## The M0 diagnostics

The spike commands are kept: each answers a question the design rests on, and
they are the diagnostics when something is wrong. `-data /data` takes every
setting left unset from the same sqlite the daemon runs on, so they need no flags
once the dashboard is configured.

| # | Question | Command | Why it could sink the design |
|---|---|---|---|
| 1 | Does the rootserver forward between its spokes? | `ping` | Both NASes are clients of the same WireGuard server. A server set up as an internet gateway will not route between them, and nothing else here can work until it does. |
| 2 | Does the tunnel come up from inside an unprivileged container? | `status` | The whole no-`NET_ADMIN` deployment rests on wireguard-go plus netstack behaving on the Synology. |
| 3 | Does DSM's WebDAV answer PROPFIND the way we assume? | `propfind`, `depth` | Href shape, namespace prefixes, and whether `Depth: infinity` is honoured. The last one is thousands of round trips per scan. |
| 4 | How long does a full metadata walk take? | `walk` | There is no remote manifest, so this is what every scan costs, forever. It sets the scan schedule. |
| 5 | Does DSM hold up under a long ranged GET? | `get`, `resume`, `soak` | Resume is the property the transfer engine is built on. If ranged GET is unreliable, a 40 GB file interrupted at 39 GB starts again from zero. |

```bash
docker compose run --rm jcc-mirror ping     -data /data -target <publisher-tunnel-addr> -count 10
docker compose run --rm jcc-mirror status   -data /data
docker compose run --rm jcc-mirror propfind -data /data -path "" -raw
docker compose run --rm jcc-mirror depth    -data /data -path "ClipCornDB"   # a SMALL subdirectory
docker compose run --rm jcc-mirror walk     -data /data -workers 8 -out /data/manifest.ndjson
docker compose run --rm jcc-mirror resume   -data /data -path "Filme/Some.Big.File.mkv" -length $((256<<20))
docker compose run --rm jcc-mirror soak     -data /data -path "Filme/Some.Big.File.mkv" -duration 4h
```

Every setting can still be given as a flag instead, and a flag always wins over
the stored value — trying something other than what is configured is the point.
`-no-tunnel` points any of them at a local WebDAV server. Run `jcc-mirror help`
for the full list.

### Reading the results

- **`ping` gets no replies but the handshake succeeds.** The tunnel is fine and
  the spoke-to-spoke route is not. Check `net.ipv4.ip_forward=1` on the
  rootserver and that its peer entries' `AllowedIPs` cover *both* clients. This
  is not a jcc-mirror bug and it is the most likely way the setup fails.
- **`depth` says infinity is refused.** Expected, and fine — RFC 4918 allows it.
  The scanner then walks one PROPFIND per directory, which is what the design
  assumes anyway. Point the probe at a *small* subdirectory: if the server does
  honour it, an infinity request on the collection root returns the entire tree
  as a single XML document.
- **`walk` reports non-NFC names.** Part of the tree passed through macOS. The
  diff must normalize, or every title with an umlaut looks new on every scan.
- **`resume` reports differing hashes.** Ranged resume is not usable against this
  server, and the transfer design needs rethinking before M2. This is the single
  most consequential result the spike can produce.
- **`soak` reports a first error at a suspiciously round time.** DSM drops long
  transfers on a timer. Not fatal — the job state machine has to treat a dropped
  connection as routine — but it decides how aggressive the retry policy is.

## Configuration

There is no config file, no `.env`, and nothing is read from the environment.
Only true invariants are compiled in (`static.go`): the data directory, the two
listen addresses, the tunnel MTU and keepalive. Everything else lives in the
`config` table, is edited in the dashboard, and every change is recorded in
`config_audit` — with secrets masked, so the trail says that a password changed
and never what it changed to.

Two settings are generated at first start and never typed by hand: the WireGuard
private key and the dashboard token. The private key therefore exists in exactly
one place and never passes through a compose file, a shell history or `ps`.

## Publisher-side setup

Nothing of ours runs there. Enable the stock DSM **WebDAV Server** package,
share the directories, and create a read-only account. HTTP on 5005 is enough:
the tunnel already encrypts and authenticates every packet, and plain HTTP is
faster through netstack.

## Development

```bash
make test        # unit tests plus end-to-end runs against a local WebDAV server
make vet
make build
./jcc-mirror serve -data ./data -lan 127.0.0.1:8080
```

Nothing in the test suite needs a network, a tunnel or a NAS. The WebDAV client
and the walk run against `golang.org/x/net/webdav` serving a temporary directory,
and `remote/localfs` serves a directory as a `remote.Remote` directly — with
whole-second timestamps, so a test cannot pass on fidelity the real remote does
not have. `TestAgreesWithWebDAV` holds the two implementations to the same
answers, which is the reason `remote.Remote` is an interface at all.

## Structure

```
main.go        command dispatch, shared flags for the diagnostics
static.go      the settings that are compiled in, and why
cmd_serve.go   the daemon
cmd_*.go       one file per diagnostic command
app/           the running daemon: tunnel lifecycle, dashboard, setup view
store/         sqlite state: migrations, config with audit, events
wg/            wireguard-go + netstack: the tunnel, ping and device status
webdav/        PROPFIND, HEAD and ranged GET; propfind.go is the pure decoder
remote/        the three-method interface the engine talks to
remote/localfs/  a local directory as a Remote, for tests and development
logs/ format/  the leveled logger and the number formatting everything shares
deploy/        Dockerfile and compose for running it on the Synology
```
