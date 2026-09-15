# Design: jcc-mirror

One-way replication of a jClipCorn collection (media + shared database) from a publisher NAS to a
subscriber NAS over WireGuard, with a web dashboard, scheduling and self-update.

Roles follow `jClipCorn/DESIGN_DATABASE_SPLIT.md`: **publisher** = user 1 (source of truth, ~30 TB),
**subscriber** = user 2 (Synology, mirror). Strictly one-way.

**Hard constraint: nothing of ours runs on user 1's NAS.** It exposes an SMB share and nothing
more. Every decision below follows from that.

---

## 0. Decisions

All settled. Nothing here is still open for debate; §9 is the build order.

| Topic | Decision |
|---|---|
| Publisher side | **No agent.** Pull-only from a plain share. No SQLite commands, no snapshotting, no remote manifest. |
| Remote protocol | **SMB2.** A userspace client over a `net.Conn` we supply, so it traverses the userspace tunnel unchanged and enumerates one round trip per *directory*. NFS was ruled out — see §2.1. |
| DB consistency | Copy `ClipCornDB.db` **only when no `.~lock` exists beside it**. Absence of the lock means a clean shutdown, so there is no writer and no hot journal — the byte copy is consistent. |
| Lock files | Existence check only. Never parse the PID, never interpret it. |
| Covers | Ordinary files in an ordinary pair. Missing covers are cosmetic, cover deletion is rare. No ordering constraint. |
| DB pre-flight | None. jcc-mirror never opens the database. Version skew is handled by mirroring the jClipCorn program directory too; DUUID pairing was done manually at setup. |
| Bootstrap | Initial ~30 TB copied manually by USB, then adopted **by size only** — mtimes are not trusted and not compared on the adopt pass. |
| Program dir vs. DB | Ordinary pair priority, no special machinery. Both are small and will generally sync in the same cycle; if they ever land out of order jClipCorn just fails to start, which is recoverable. |
| Content awareness | Zero. jcc-mirror moves bytes and does not know what is in them. |
| Topology | Hub-and-spoke: a rootserver runs the WireGuard server, both NASes are clients. Endpoint is a stable IP, so no DNS re-resolution is needed. |
| Notifications | SCN (`simplecloudnotifier.de`), configured in the dashboard. See §4.1. |
| Update trust | No signing. Fetch from one of the shares, or from an absolute URL, compare timestamps, smoke-test, keep the previous binary. |
| Module path | `blackforestbytes.com/jcc-mirror` |

**Safety rules carried into the build.** Uncontested, listed once so they do not get lost in the
detail sections:

| # | Rule | Why |
|---|---|---|
| S1 | Re-check the source lock *after* copying the DB; discard if it appeared. | One extra request; closes the mid-copy race. Not a SQLite command. |
| S2 | Deletion threshold + quarantine + non-empty-remote assertion. | A publisher-side share going away makes the entire tree look deleted. |
| S3 | The dashboard's reach *is* its access control: bind explicitly, never publish `:8080` past the LAN. | There is no authentication, so "force replace" and "approve deletion" are exactly as protected as the network they answer on. See §4. |
| S4 | Free-space preflight, per plan and per file. | 30 TB may not fit; a full volume on a Synology is its own kind of bad day. |
| S5 | Keep the last N copies of `ClipCornDB.db`. | A few MB each. The entire recovery story for a bad transfer, which is the only realistic failure mode left. |

**Forfeited by the constraints**, noted so it reads as a choice rather than an oversight: selecting
what to mirror by ClipCorn metadata (group, score, genre) is off the table, since that needs to read
the DB. Path include/exclude globs are the only selection mechanism.

---

## 1. Facts established from the existing code

**The lock file is `.~lock`** — `FileLockManager.java`. The path is `<dbfile>.~lock`, i.e.
`ClipCornDB.db.~lock`, not `*.lock`. The body is a bare PID, which is exactly the part to ignore:
read from another machine it is uninterpretable.

**"No lock file" really does imply "safe to copy".** The lock is written on open and deleted on
clean shutdown. A crash leaves *both* the lock and any `-journal` behind, so the two conditions move
together: no lock ⇒ clean shutdown ⇒ no hot journal ⇒ the `.db` file alone is a complete, consistent
database. The rule is self-consistent and needs no SQLite involvement.

The residual case is a **stale** lock — user 1's ClipCorn crashed and was never restarted, so the
lock sits there forever and the DB silently never syncs. Do not auto-steal it. Surface it: if the
lock's mtime has been unchanged past a configured age, raise a dashboard warning with a manual
override button.

**On-disk layout** — `DESIGN_DATABASE_SPLIT.md` §3; the driver builds `<dbDir>/<dbName>/<dbName>.db`:

```
<workdir>/ClipCornDB/
├── ClipCornDB.db          shared   — synced
├── ClipCornUserData.db    per-user — never synced
├── ClipCornHistory.db     per-user — never synced   ← easy to miss
└── cover/                 shared   — synced, ${uuid}.${ext}
```

Three databases, not two. The main DB's name derives from the configured `dbName`, so it belongs in
config rather than hardcoded.

**Journal mode is rollback, deliberately.** The driver carries an explicit comment against WAL:
atomicity across the attached `main`/`userdata` pair requires it. Consequence for us: a
`ClipCornDB.db-journal` can exist, and must never be transferred — a journal without its exact
matching database is worse than useless.

**Prior art.** `ClipCorn/jellyfin-sync` is the house style to follow — Go, `CGO_ENABLED=0`,
`modernc.org/sqlite` for local state, a `Makefile` with cross-compile targets, a `deploy/` directory.

---

## 2. Architecture

One binary, one deployment, on user 2's Synology. The publisher is a `\\host\share\path`.

```
   user 1 NAS                  WireGuard                user 2 Synology
 ┌──────────────┐                                 ┌──────────────────────────────┐
 │ DSM SMB      │◄── QUERY_DIRECTORY (per dir) ───┤ jcc-mirror (docker)          │
 │              │◄── existence probe (lock) ──────┤  wireguard-go netstack        │
 │  /media      │◄── read at offset (resumable) ──┤  scanner → manifest table     │
 │  /ClipCornDB │                                 │  differ → jobs table          │
 │  /jClipCorn  │                                 │  transfer engine + limiter    │
 │  /dist       │                                 │  scheduler · guards · events  │
 │              │                                 │  sqlite state                 │
 │ stock DSM    │                                 │  dashboard (angular, embed)   │
 │ package;     │                                 └──────────────────────────────┘
 │ our code:    │
 │ none         │
 └──────────────┘
```

### 2.1 Remote access: SMB

SMB is what user 1's DSM actually serves, and `github.com/hirochachacha/go-smb2` is a client that
never touches the kernel — it speaks the protocol over a `net.Conn` the caller hands it. That is the
whole argument:

- It goes through netstack unchanged — `smb.Config{Dial: wg.Tunnel.DialContext}`, and the library
  dials nothing itself. The unprivileged-container property survives: no mount, no `CAP_SYS_ADMIN`,
  no kernel module.
- One `CREATE` plus `QUERY_DIRECTORY` returns every child of a directory with size and mtime in
  **one round trip per directory**, not per file. Thousands of requests for the collection, not
  hundreds of thousands.
- A file handle seeks, so a bounded read at an offset gives resume for free, across restarts and
  self-updates.
- Failures are NTSTATUS codes. Four of them mean "there is nothing at that path" —
  `STATUS_NO_SUCH_FILE`, `STATUS_OBJECT_NAME_NOT_FOUND`, `STATUS_OBJECT_PATH_NOT_FOUND` and
  `STATUS_NOT_A_DIRECTORY` — and they are mapped explicitly rather than by a catch-all, because the
  lock gate of §3 must never read "cannot tell" as "nobody is using it". Anything else stays an
  error and fails the gate closed.
- Cost on user 1's side: enable SMB under DSM's **File Services**. A checkbox, not our code.

*Why not NFS*, recorded so the decision does not get revisited: a kernel NFS mount needs a real
network interface, so it cannot go through the userspace tunnel — that forces TUN WireGuard,
`/dev/net/tun`, `NET_ADMIN` and host-side setup. Mounting NFS inside a container needs
`CAP_SYS_ADMIN`; mounting it on DSM and bind-mounting in means the sync silently stops whenever the
mount goes stale. And NFSv3 issues roughly a GETATTR per entry, making a 100k-file walk a matter of
hours. None of that is softened by the remote now speaking SMB: every objection is to the *kernel
mount*, and a `cifs` mount inherits all three. What is used here is a library in our own process
reading a socket we opened.

Keep the remote behind a small interface anyway — it makes the whole engine testable against a local
fake with no network, which is worth more than the protocol flexibility:

```go
type Remote interface {
    List(ctx context.Context, dir string) ([]Entry, error)
    Stat(ctx context.Context, path string) (Entry, error)
    Open(ctx context.Context, path string, offset int64) (io.ReadCloser, error)
}
```

Three methods, and they stay three because that is what a fake has to implement. The two things
that need more than three are extensions a caller type-asserts for: `Prober`, one `Exists` for the
lock gate of §3, so the gate does not list a directory of ten thousand covers to learn about one
file; and `RangeReader`, one `OpenRange(path, offset, length)` for the transfer engine, because
without a length every parallel stream within a file would run to the end of it and the whole file
would come down once per stream.

Practical SMB notes for the implementation: the session goes to port 445 unless the remote's host
says otherwise; the share is named the way DSM names it, on its own, with no server prefix and no
path; an NTLM domain is optional and a DSM-local account needs none. One session per remote is
opened lazily and kept — mounting per operation would put a handshake in front of every directory of the walk —
guarded by a mutex so two callers arriving at once do not open two connections, and re-dialed
exactly once on a connection-level failure, so a tunnel blip does not fail a sync that has been
running for days. Only transport failures are retried that way: an NTSTATUS is the server answering,
and asking it twice gets the same answer. Names are normalized to NFC as they are read, since that
is the form the manifest compares in (§2.3).

### 2.2 WireGuard without touching the host

`golang.zx2c4.com/wireguard/tun/netstack` — wireguard-go with a gVisor userspace TCP/IP stack. The
tunnel exists only inside the process:

```go
tun, tnet, _ := netstack.CreateNetTUN([]netip.Addr{wgIP}, []netip.Addr{dns}, 1420)
dev := device.NewDevice(tun, conn.NewDefaultBind(), logger)
dev.IpcSet(uapiConfig)   // private key, peer pubkey, endpoint, allowed-ips, keepalive
dev.Up()

smbc, _ := smb.New(smb.Config{Dial: tnet.DialContext, ...})   // the remote, on the tunnel
ln, _   := tnet.ListenTCP(&net.TCPAddr{Port: 8080})           // dashboard, on the tunnel
```

Consequences:

- No `NET_ADMIN`, no `--privileged`, no `/dev/net/tun`, no host-side WireGuard on DSM. An ordinary
  unprivileged container.
- The WireGuard identity belongs to the container, so moving it between hosts is a volume copy.
- `tnet.ListenTCP` puts the dashboard **on the tunnel**, so user 1 reaches it over WireGuard with
  zero configuration on user 2's side — no port forward, no host WG. It is also the second half of
  S3: the dashboard has no authentication, so everyone on the WG subnet has "force replace" and
  "approve deletion" as surely as everyone on the LAN does. That is an accepted cost, argued in §4.

**Topology.** The WireGuard server runs on a rootserver; user 1's NAS and user 2's container are
both clients of it. Three consequences:

- The peer entry is the *rootserver* — stable public IP, so the endpoint never needs re-resolving.
  (Worth noting because `wireguard-go` resolves the endpoint once at `IpcSet` and never again; a
  dyndns endpoint would have needed explicit re-resolution on handshake failure. It does not.)
- `AllowedIPs` must cover the **WG subnet**, not just user 1's address, since traffic to his NAS is
  routed via the server. `0.0.0.0/0` is accepted too, and harmless, which is the one place a
  userspace tunnel differs usefully from a kernel one: there is no host routing table behind
  netstack for a default route to hijack, so the prefix only decides which traffic *already handed
  to the tunnel* goes to this peer. Since the server's own config routinely says `0.0.0.0/0, ::/0`,
  refusing it would have meant hand-editing every config we import.
- **The rootserver must route client-to-client.** `net.ipv4.ip_forward=1`, and its peer entries'
  `AllowedIPs` must cover each client's address. A server configured purely as an internet gateway
  will not forward between spokes, and the same requirement applies in reverse for user 1 reaching
  the dashboard. This is the single most likely reason M0 fails, and it is not a jcc-mirror bug —
  verify it first with a plain `ping` between the two WG addresses.

All traffic therefore hairpins through the rootserver, so **its uplink and any monthly traffic
allowance are the real ceiling**, not either NAS's connection. The 30 TB bootstrap goes by USB so
this mostly matters for steady-state volume, but it is worth knowing the number before setting the
bandwidth schedule.

Practical details that decide whether it works: the MTU (1420 by default, and the usual cause of
"WireGuard is mysteriously slow"), and `PersistentKeepalive` (25 s by default, since both clients
are behind NAT). Both are settings rather than constants — `wg.mtu` and `wg.keepalive` — because
they are exactly the two numbers the server's own config file has an opinion about, and the config
is imported wholesale (§6). The defaults are what a config that says nothing about them gets;
changing either re-opens the device. Netstack throughput is CPU-bound — expect roughly
300–800 Mbit/s on a low-end Synology CPU, which will not be the bottleneck.

### 2.3 Scanner and differ

No remote manifest exists, so jcc-mirror builds one itself — and the walk, not the transfer, becomes
the operation that needs care:

- Walk each pair's remote root by one SMB directory listing per directory, breadth-first, into a
  `manifest` table: `(relpath, size, mtime, is_dir)`. Bounded concurrency (~4–8 in flight) — the
  walk is latency-bound, not bandwidth-bound, so a little parallelism helps a lot and more helps
  nothing.
- Diff `manifest` against `files` (local truth) on **size and mtime**. Media is immutable; a file is
  new, unchanged, or gone.
- Persist the manifest so a scan interrupted by a window boundary or a restart resumes rather than
  restarting, and record per-scan duration so the dashboard shows what a scan actually costs.
- No source-side checksums are available over SMB, and that is fine: WireGuard authenticates
  every packet (ChaCha20-Poly1305) on top of TCP checksums, so silent in-transit corruption is not a
  realistic failure mode. Truncation is, and a size check catches it. Optionally hash locally after
  download and store it in `files` for a future background scrub.

**Three ways the diff can silently break**, each producing the same symptom — every file looks
changed on every scan, forever, and 30 TB re-transfers. All belong in M2:

- SMB reports mtime as a FILETIME — 100-nanosecond ticks, so the wire is not the lossy part here.
  The **manifest** is: it stores whole milliseconds, like every other timestamp in the database, and
  local timestamps are nanosecond. A round trip through the manifest therefore truncates, and the
  destination volume has a granularity of its own on top. Compare with a tolerance (≥1 ms; the
  default is 2 s, which also survives a FAT-formatted intermediate) rather than for equality. Zero
  is never right.
- After a download completes, `os.Chtimes` the file to the *remote's* mtime before the rename.
  Without it the local mtime is the download time, and the next scan sees a change.
- Normalize paths to NFC on both sides before comparing. If any part of the tree passed through
  macOS the names may be NFD while the source is NFC, and every title with an umlaut looks new.

### 2.4 Transfer engine

- **A bounded read at an offset** into `<destdir>/.jccmirror/<sha256(relpath)>.part`, then verify
  size, then `rename(2)`. Deterministic names, not random ones: a random name cannot be found again
  after a restart, which throws away resume. The hidden directory keeps partials out of Jellyfin and
  ClipCorn scans, and living under the destination root keeps the rename same-filesystem, which is
  what makes it atomic.
- **Concurrency**: one *file* at a time, as intended — but 2–4 streams per file, each a separate
  file handle reading its own range. A single TCP stream over a high-BDP link with any loss will not
  fill the pipe. `chunks=1` reproduces strictly-sequential behaviour. The streams share the one SMB
  session rather than each taking a connection out of a pool, which is why a length on the read is
  not optional: without it every stream would run to the end of the file and the whole file would
  come down once per stream.
- **Bandwidth limit**: an `x/time/rate` limiter shared across chunk readers, so the cap is global
  rather than per-stream, and adjustable live when the scheduler crosses a window boundary.
  *Measurement* sits lower, on the dialled connection itself — there is no round trip to hook once
  the remote is not HTTP — so the SMB framing and the walk's directory listings are counted too. That
  is the right number anyway: the Bandwidth view answers "what did this cost the link", not "how many
  file bytes landed".
- **Retry**: every file is a row in `jobs` with an explicit state machine
  (`pending → running → verifying → done | failed`), attempt counter and exponential backoff. A
  crash, a self-update, or a closed transfer window just leaves rows in `running`, requeued on
  startup. That, rather than an in-memory retry loop, is what makes it retriable.
- **Adopt mode** — for the USB bootstrap, and needed in M2 rather than later. Walk the local tree,
  match against the remote manifest **on size alone**, and mark matches `done`, recording the
  remote's mtime as local truth. Mtimes are not compared, because a USB copy that did not preserve
  them would otherwise make all 30 TB look changed. Report the count and total size adopted so the
  result is verifiable before the first sync runs.

### 2.5 Deletion safety (S2)

Unattended deletion of a 30 TB dataset is the one operation here that destroys something
irrecoverable:

1. **Threshold** — refuse a plan that deletes more than *N* files or *X* % of the pair; pause it and
   surface it for one-click approval.
2. **Delete after, never during** — the tree only shrinks once the additions succeeded.
3. **Quarantine** — move to `<destdir>/.jccmirror/trash/<date>/` with a retention (default 7 days)
   rather than unlinking.
4. **Non-empty assertion** — a pair whose remote root does not exist or lists empty is an error, not
   "everything was deleted".

### 2.6 Free space (S4)

Preflight every plan against `statfs` and refuse to start if completion would drop below a reserve
(default 50 GB). Check per file too, since a plan can run for days. If the collection does not fit,
path-glob excludes are the only lever available.

---

## 3. The jCC pair

An ordinary directory sync of `<workdir>/ClipCornDB/` plus two rules. `cover/` is not special — it
syncs as ordinary files in the same pair.

**1 · Hard exclusions.** Refusals, not user-editable patterns, because no configuration makes them
correct:

| File | Why |
|---|---|
| `ClipCornUserData.db` | Per-user. Overwriting it destroys user 2's ratings, tags and filters. |
| `ClipCornHistory.db` | Also per-user. |
| `*.db-journal` `-wal` `-shm` | Transient, and meaningless apart from their exact matching database. |
| `*.~lock` | Belongs to whoever is running. |

**2 · A lock gate on `ClipCornDB.db` only.** The rest of the pair transfers regardless:

```
1. probe <remote>/ClipCornDB.db.~lock   → present ⇒ skip this cycle, note it
2. stat  <local>/ClipCornDB.db.~lock    → present ⇒ skip this cycle, note it
3. read  <remote>/ClipCornDB.db → .ClipCornDB.db.incoming   (staged in the target dir)
4. verify size against the size the remote reported
5. probe <remote>/ClipCornDB.db.~lock   again  → appeared ⇒ discard, retry later   (S1)
6. stat  <local>/ClipCornDB.db.~lock    again  → appeared ⇒ discard, retry later
7. keep the current file as a numbered backup                                      (S5)
8. rename(.ClipCornDB.db.incoming → ClipCornDB.db)
```

The remote probe is `Remote.Exists` — an existence check and nothing more, since the body of a
`.~lock` is a PID from another machine. It fails closed by construction: only the four NTSTATUS
codes that mean "there is nothing at that path" become `false`, and every other error is an error,
so a refused request or a tunnel that went down can never be read as "nobody is using it" (§2.1).

Step 5 is the only non-obvious one: user 1 can launch jClipCorn during the copy, and one extra probe
closes that window. Steps 3 and 8 stage in the *target directory* so the rename is same-filesystem
and atomic — the local exposure is one syscall rather than the duration of a copy.

The jClipCorn program directory is just another pair with a lower priority number, so it is normally
transferred in the same cycle, ahead of the DB. No special machinery: if the two ever land out of
order, jClipCorn refuses to start and the next cycle fixes it.

---

## 4. Dashboard and notifications

Angular, `ng build` → `//go:embed all:web/dist`, SPA fallback route, dev mode proxying to
`ng serve`. Live data over **Server-Sent Events** — one-way, reconnects for free, survives any proxy.
Events are the same rows written to the `events` table, so the live view and the history view share a
schema.

| View | Contents |
|---|---|
| **Now** | Current pair and file, progress, instantaneous and average rate, ETA, queue depth, active window and limit. |
| **Changes** | Per-file history — added, replaced, deleted, failed — filterable by pair and time. |
| **Events** | Scan started/finished with duration, sync finished, DB replaced, DB skipped (locked), update applied, restart, error, deletion guard tripped. |
| **Bandwidth** | Per-minute buckets, rolled up to hourly after ~7 days and daily after ~90 — otherwise the table grows without bound. A time series, plus the hour-of-day cumulative, best drawn as a 7×24 heatmap so the weekday/weekend shape is visible. |
| **Config** | Everything in §6, with an audit trail. |
| **Diagnostics** | WireGuard handshake age and endpoint, RTT, a throughput probe, reachability of each remote reported against the `\\host\share\path` it actually reached, a directory-listing explorer for any remote's tree, free space, "explain plan" per pair, live log tail. |

Config is one item in that nav and one route, but not one page: the settings groups are split over
nine tabs — Connection, Remotes, Schedule, Pairs, Transfer, jCC, Notifications, System, Audit — each holding
its own draft and its own save bar, so saving on one tab can never rewrite a field on another. A
route-to-group table is the single place membership is written down, and a group no tab claims lands
on System rather than disappearing, which is what keeps a key added on the Go side from falling out
of the dashboard unnoticed. Pairs and Audit have no save bar: a pair is a record saved one at a
time, and the audit trail is read-only.

**Actions**: trigger scan, trigger sync, pause/resume, approve a guarded deletion, force a DB
replace, roll back a DB backup, check for update.

**Auth (S3).** There is none, and that is a decision rather than an omission. The dashboard answers
on the LAN and inside the WireGuard tunnel and nowhere else, and a bearer token would move that
boundary nowhere: with no user accounts to authenticate against, the only secret the daemon can
issue is one it generates and prints to the container log — which is readable by exactly the people
the port already is, so it costs a lookup before every change and excludes nobody. The boundary that
is actually doing the work is the reachability of the port, so that is what S3 protects: **bind
explicitly** to the LAN listener and the netstack listener rather than `0.0.0.0`, do not publish
`:8080` past the LAN in compose, and run no CORS, so a page on another origin cannot drive the API
from a browser that can reach it.

The cost is stated plainly because it is real: **"force replace" and "approve deletion" are
reachable unauthenticated** from any device on the LAN and from anything on the WG subnet. The
mitigation is that the WG subnet is two NASes and a rootserver we control, and that the destructive
paths are guarded on their own terms rather than by a login — deletion is thresholded, quarantined
and needs an explicit approval (§2.5), a DB replace keeps numbered backups it can be rolled back
from (§3), and every configuration change is written to an audit trail with the caller's address.
Publishing that port to the internet is the one deployment mistake this design cannot absorb.

The dashboard does keep a **read-only mode**, and it is honest about being a guard rail rather than
a control: it lives entirely in the browser, is remembered in `localStorage`, defaults to locked,
and is toggled by one Unlock/Lock button in the header. Every mutating control is disabled while it
is locked. It stops a stray click on a page left open, and it stops nothing else — the daemon does
not know it exists.

### 4.1 Notifications (SCN)

The dashboard is pull-only, so without a push channel a stale source lock or a dead tunnel is
invisible until someone goes and looks. SCN closes that.

`POST https://simplecloudnotifier.de/` (`/send` is the same handler), parameters as query, form or
JSON body:

| Field | |
|---|---|
| `user_id` (int), `user_key` (string) | required — note it is a **pair**, not a single key |
| `title` (string) | required |
| `content`, `channel`, `sender_name` | optional |
| `priority` | 0 / 1 / 2 |
| `msg_id` | dedup / idempotency key |
| `timestamp` | unix |

Two properties of that API drive the design:

**`403` means the daily quota is exhausted.** Notifications are a finite resource, so jcc-mirror must
never emit one per file. Everything is coalesced to at most one message per *sync run* or per
*state transition* — "run finished: 412 files, 2 failed", not 2 failures. A condition that persists
(source lock still held, tunnel still down) notifies on the **edge**, once, and again only when it
clears.

**`msg_id` is an idempotency key**, so use it rather than tracking sent-state locally: a deterministic
hash of `(event kind, pair, day)` makes a retry after a network blip free and makes a repeated daily
alert collapse on the server side. Belt and braces against the quota.

Configurable in the dashboard: `user_id`, `user_key`, `channel`, `sender_name`, and a per-event
toggle over:

| Event | Default | Priority |
|---|---|---|
| Sync run failed, or finished with failures | on | 1 |
| Deletion guard tripped, awaiting approval | on | 2 |
| Free space below reserve | on | 2 |
| Source lock stale past the threshold | on | 1 |
| Tunnel down longer than N minutes | on | 1 |
| `ClipCornDB.db` replaced | on | 0 |
| Self-update applied, or rolled back | on | 0/2 |
| Sync run finished cleanly | off | 0 |

A failed notification is written to `events` and never fails a sync — it is telemetry, not a step.

---

## 5. Self-update

"Update in place without restarting docker" is not literally possible — the kernel holds the
executable's inode. What is possible gives exactly the desired property, and in the usual setup the
update binary lives on an SMB share the mirror already reads, so it needs nothing new on user 1's
side:

1. Stat the binary and compare its mtime against the build timestamp compiled in via `-ldflags`.
   Newer ⇒ update. No version file, no manifest — literally "is the file there newer than mine".
   `update.url` is a share-relative path (`dist/jcc-mirror-amd64`) in the usual form, read off the
   remote `update.remote` names — the first remote when it is empty — through the same client the
   mirror uses for it; an absolute `http(s)` URL is fetched over the tunnel instead, with that
   remote's account, and there the same comparison is a `HEAD` and its `Last-Modified`. Empty
   switches updating off.
2. Download to `/data/bin/jcc-mirror.new` and check it is plausible before trusting it: the size
   matches what the stat said, and the first four bytes are the ELF magic. This is not signing — it
   is catching a truncated download or an HTML error page saved as a binary, which is a different
   and much more likely failure than a malicious one.
3. Smoke-test: run `jcc-mirror.new --version` as a subprocess and require a sane exit.
4. Quiesce — or just stop, since transfers are resumable by construction.
5. `rename(jcc-mirror.new → jcc-mirror)`, keeping the old one as `jcc-mirror.prev`.
6. `syscall.Exec(self)` — same PID, same container, no Docker restart. That is the whole trick.

Safety net: the container entrypoint is a three-line supervisor that restores `jcc-mirror.prev` if
the new binary exits non-zero within 60 s of a self-update. Steps 2, 3 and the supervisor are the
whole trust story, and they are aimed at corruption rather than tampering — the transport is a
private tunnel to a machine you control. Auto-update is a toggle; when off, the dashboard shows
"update available" with a button.

---

## 6. Configuration

There is no bootstrap layer. Nothing is read from the environment and no config file is mounted:
every setting is either **static** — compiled in, because it does not vary between installs — or
**runtime**, held in sqlite and edited in the dashboard.

**Static**: the data directory (`/data`, the volume mount) and the two dashboard listen addresses
(LAN and the netstack listener on the tunnel).

**The WireGuard server's config is the input**, and that direction is the decision. The rootserver
issues a *finished client config* — it assigns the address, it chooses the private key — and there
is no step in that workflow where an operator can hand it a public key we generated and have it
accepted. Generating our own keypair and asking for it to be pasted into a peer entry therefore
describes a deployment that does not exist. So `[Interface] PrivateKey` is an ordinary editable
secret like any other setting, and the primary path is **import**: paste the `wg-quick` file into a
textarea, and one request spreads it over all nine tunnel settings — private key, peer public key,
preshared key, endpoint, address, allowed-ips, DNS, MTU, keepalive — through the same validated,
audited write any other config change takes.

Three properties of that import earn their keep. It writes **all nine, blanks included**, because
the file is the whole truth about the tunnel: a config that does not mention a preshared key has to
*clear* the stored one, not leave it behind to be silently mixed into a tunnel it does not belong
to. It **names back what it skipped** rather than failing or ignoring in silence: `ListenPort`,
`Table`, `PreUp`/`PostUp`/`PreDown`/`PostDown`, `SaveConfig` and `FwMark` are directives for a
host-side wg-quick and a userspace netstack cannot honour any of them, so the operator is told which
lines had no effect. And it refuses a file with **more than one `[Peer]`** — this talks to exactly
one server, and quietly picking the first peer would be the kind of guess that fails a week later.

A private key is still **generated at first start**, into sqlite, but only as a stand-in: it makes
the tunnel settings a complete, valid record before anything is imported, and it is meant to be
overwritten. The dashboard shows the public half of whatever key is stored, which after an import
is the server's own idea of this client and is worth being able to check against the peer entry.

That leaves one loop to close, and it closes itself: the LAN listener does not depend on the tunnel,
so the dashboard answers on `:8080` before any WireGuard setting exists. A first boot against an
empty database serves a setup view, the config is pasted in, and the tunnel comes up the moment it
is saved. Nothing has to be known before the process starts.

**Runtime** (sqlite, edited in the dashboard, every change written to an audit table):

- **Tunnel**: our private key, the rootserver's public key, endpoint, our address inside the tunnel,
  allowed-ips, optional preshared key, optional DNS, MTU, keepalive — the nine the import writes.
  Changing any of them re-opens the tunnel in place.
- **Pairs**: `{id, name, type: raw|jcc, remote_id, remote_path, local_path, mode: mirror|additive,
  includes[], excludes[], delete_guard, priority, enabled}`. `remote_id` is the remote the pair
  reads from and `remote_path` is relative to that remote's root. A pair with no remote is a valid
  record that nothing runs until it is given one.
- **Schedule**: one 7×24 grid, one cell per weekday-hour, each carrying *both* "may transfer" and a
  bandwidth cap. The same grid answers "when to download" and "bandwidth limits", and gives
  "unlimited 02:00–08:00, 5 MB/s otherwise" for free. A separate, coarser schedule for scans, which
  cost real time. The timezone is a config value of its own, not `TZ`; render the grid in that
  zone and label it.
- **Remotes**: one record per share, as many as there are shares to read — `{id, name, host (with
  an optional port, 445 otherwise), share, path inside the share that is the remote's root, user,
  password, optional NTLM domain}`. They are rows of their own, like the pairs, rather than
  settings. The password is stored in plaintext and never handed out by the API, which says only
  whether one is set — it is a read-only account reached over a private tunnel, so plaintext at rest
  is proportionate. The first remote is the default wherever none is named: the diagnostics and
  `update.remote`. A remote cannot be removed while a pair still reads from it.
- **Transfer**: chunk count and size, retry and backoff, free-space reserve, walk concurrency.
- **jCC**: DB directory, DB name, lock staleness threshold, backup retention.
- **Update**: where the binary comes from — a share-relative path, which is the usual form, or an
  absolute URL — and the remote it is read from or fetched with (`update.remote`, the first when
  empty), auto or manual, check interval.
- **Notifications**: SCN `user_id`, `user_key`, `channel`, `sender_name`, and the per-event toggles
  from §4.1.
- **Retention**: `events` and `changes` rows, default one year.

**Dry run**: every plan can be computed and displayed — N adds, M deletes, X GB, estimated duration
at the current cap — without executing. Worth an "approve each plan" mode for the first weeks.
*Plan* diffs against the last walk and shows a sample; *Dry run* walks first, with the walk's live
counters, and keeps the complete list — latest run per pair, in memory, paged by
`GET /api/runs/dryrun` rather than carried on the stream. It writes the manifest and nothing else.

---

## 7. State (sqlite, `modernc.org/sqlite`, WAL)

```
config(key, value, updated_at)            -- with config_audit(key, old, new, ts)
remotes(id, name, host, share, path, username, password, domain)  -- one per share
pairs(..., remote_id, ...)                -- the remote it reads from; NULL is none yet
files(pair_id, relpath, size, mtime, hash, state, verified_at)   -- local truth
manifest(pair_id, relpath, size, mtime, is_dir, seen_at)         -- last remote walk
scans(id, pair_id, started_at, finished_at, entries, state)      -- resumable walks
jobs(id, pair_id, relpath, op, bytes_total, bytes_done,
     state, attempts, next_attempt_at, error)
events(ts, level, kind, pair_id, message, data_json)
changes(ts, pair_id, relpath, op, size_before, size_after)
bw_samples(ts_minute, bytes_in, bytes_out)                       -- rolled up on a schedule
db_backups(ts, path, size, hash)                                 -- jCC DB rollback
```

WAL is right for *this* database — unlike ClipCorn's, it has no attached second file to stay atomic
with.

---

## 8. Deployment

```yaml
services:
  jcc-mirror:
    image: jcc-mirror:latest
    restart: unless-stopped
    ports: ["8080:8080"]              # LAN dashboard; WG dashboard is on the netstack listener.
                                      # Do not publish it beyond the LAN: there is no auth (S3, §4)
    volumes:
      - /volume1/docker/jcc-mirror:/data          # sqlite, config, wg key, binaries, db backups
      - /volume1/media:/mnt/media
      - /volume1/clipcorn:/mnt/clipcorn
    user: "1026:100"                  # uid/gid that owns the media share; no env, see §6
```

No `cap_add`, no `devices`, no `network_mode: host`. The Synology permissions trap: the container
must write as a uid/gid that owns the media share, or every rename fails at the last possible
moment — hence compose's `user:`, which Docker applies before the process starts, so there is no
privilege-dropping entrypoint and no `PUID`/`PGID` to pass.

Base image `gcr.io/distroless/static` or alpine; the binary is static (`CGO_ENABLED=0`, pure-Go
sqlite). Module path `blackforestbytes.com/jcc-mirror`; build `linux/amd64` and `linux/arm64`.

Setup elsewhere, once:

- **Rootserver**: add user 2's container as a WireGuard peer, and confirm it forwards between spokes
  (§2.2).
- **User 1's NAS**: enable SMB under DSM's File Services, share the directories, create a read-only
  account — SMB has no usable anonymous mode, so one is needed even for a share open to everyone.
  Nothing else — no binary, no cron, no code.

---

## 9. Milestones

| # | Deliverable | Why here |
|---|---|---|
| **M0** | **Spike: netstack WireGuard + SMB.** In order: `ping` user 1's WG address to prove the rootserver forwards between spokes; join from a container on the Synology via netstack; `ls` the collection root over the tunnel; **time a full metadata walk of the real tree**; then a **multi-GB read, a resume from a byte offset mid-file, and a transfer left running for hours** to see whether DSM's SMB holds up; confirm user 1 can reach a test listener over the tunnel. | Everything that could invalidate the design, in one afternoon. The spoke-to-spoke route and DSM's behaviour on a long-lived session are the two most likely to bite, and the walk time sets the scan schedule. |
| M1 | Skeleton: binary (`blackforestbytes.com/jcc-mirror`), sqlite state, config table + setup view, `Remote` interface + SMB implementation + local fake, events, `/healthz`. | |
| M2 | Scanner, differ, transfer: resumable walk, bounded reads at an offset, `.part` files, verify, rename, job state machine, retry — **and adopt mode**, without which the USB bootstrap can't be recognised. | The core, testable from the CLI with no UI. |
| M3 | Scheduler + limiter: 7×24 grid, caps, window boundaries mid-transfer, free-space preflight. | |
| M4 | Deletion + guards: mirror mode, threshold, quarantine, retention, non-empty assertion. | Deliberately after M2/M3 — run additive-only until the diff is trusted. |
| M5 | jCC pair: hard exclusions, lock gate with the re-check, staged rename, numbered backups + rollback. | Small once M2 is solid. |
| M6 | Dashboard: Angular + embed, SSE, six views, the config tabs, bandwidth rollups, actions, SCN notifications. | Notifications ship with the dashboard rather than later — until then a stopped sync is invisible. |
| M7 | Self-update: mtime check, sanity checks, re-exec, supervisor fallback. | Last — a broken updater is the one bug that is hard to recover from remotely. |

---

## 10. Operational inputs still needed

Not architectural — numbers and paths needed while building, not decisions that change the design.

1. **Which directories are pairs**, and what are the paths on each side? Media, `ClipCornDB/`, the
   jClipCorn program directory, and `dist/` inside the exported share for the update binary —
   anything else?
2. **Does the whole ~30 TB fit** on user 2's NAS? If not, path-glob excludes are the only lever.
3. **Does the rootserver have a monthly traffic allowance?** Everything hairpins through it, so its
   limit — not either NAS's line — is what the bandwidth schedule has to respect.
4. **How large is the tree, in files and directories?** M0 answers this by measurement; it decides
   whether a scan takes two minutes or two hours, and therefore how the scan schedule is set.
