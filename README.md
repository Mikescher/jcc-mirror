# jcc-mirror

One-way replication of a jClipCorn collection from the publisher's NAS to the
subscriber's Synology. See `DESIGN.md` for the design; this README covers what is
built so far.

**Status: M5.** It mirrors, on a schedule, at a rate you choose; it can shrink as
well as grow; and it now handles the jClipCorn database itself. On top of M1's
skeleton — sqlite state, a config table with an audit trail, a setup view, the
`Remote` interface with a WebDAV implementation and a local fake, an event log
and `/healthz` — M2's engine — a resumable walk that builds the manifest the
publisher does not have, a differ, a transfer with ranged GETs, `.part` files,
verification, an atomic rename and a per-file job queue with retries, plus adopt
mode for the USB bootstrap — M3's scheduler — one 7×24 grid that says both when
bytes may move and how fast, a shared limiter that a window boundary adjusts
mid-transfer, and a free-space preflight — and M4's deletion, behind every guard
of `DESIGN.md` §2.5 — mirror mode per pair, two thresholds that hold a large
deletion for one approval, a dated quarantine with a retention instead of an
unlink, and the assertion that an empty manifest is a publisher who is not there
— there is now the **jCC pair**: hard exclusions that never copy the per-user
databases, a lock gate on the shared one that is re-checked after the copy, and a
numbered backup of every database it replaces, with a rollback that works even
with the tunnel down.

A pair is additive until it is told otherwise, which is the point: run
additive-only until the diff is trusted. The dashboard is M6 and self-update is
M7.

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
| `GET /api/status` · `/api/config` · `/api/config/audit` · `/api/events` · `/api/pairs` · `/api/runs` | Read views. Secrets are never returned; `status` carries the current window and cap |
| `POST /api/config` · `/api/remote/probe` · `/api/login` | Token required |
| `POST /api/pairs` · `/api/pairs/update` · `/api/pairs/delete` | Token required. JSON or a form; a JSON body may carry one key and changes only that |
| `POST /api/pairs/database/rollback` (`id`, optional `backup`) | Token required. Puts a kept copy of a jcc pair's database back; answers when it is done |
| `POST /api/runs` (`kind`, `pair`, optional `force`) | Token required. Answers as soon as the run has started, never when it has finished. `force` is the lock-gate override |

## The mirror

Two ways in, on the same state: the dashboard and the CLI.

**From the dashboard.** The setup view has a Mirror section: the pairs with how
far behind each one is, an editor for them, and a Plan / Scan / Adopt / Delete /
Database / Sync button each — plus, when a deletion is over a threshold, the two
buttons that answer it, and for a jcc pair the copies of its database with a
rollback beside each one. A run happens in the background and the page refreshes
itself while one is going, so a sync that takes a day and a half is watchable
from a phone.
One runs at a time — the transfer engine moves one file at a time by design, and
the scheduler walks the pairs in priority order for the same reason. Stop is
always safe: a walk stays resumable and every transfer keeps its place in the
file.

This is the route to use if the container's shell is awkward to reach, which on a
Synology it usually is. It is throwaway UI — M6 replaces it with the real
dashboard — but the bootstrap has to be reachable long before M6.

**From the CLI.** Every command takes `-data`, so they all work on the same sqlite
state the daemon runs on, and `-remote-dir` points any of them at a local
directory instead of the publisher's share — the whole milestone can be exercised
with no NAS, no tunnel and no WebDAV server.

A pair is one directory over there mapped onto one directory here. Nothing is
mirrored until one exists:

```bash
jcc-mirror pairs add -data /data -name media \
    -remote "Filme" -local /mnt/media/Filme -mode additive \
    -exclude "**/*.tmp,Serien/Trash/**"
jcc-mirror pairs -data /data
```

Then the three steps, in order. They are separate commands because each answers
a different question, and because the first weeks are meant to be run by hand:

```bash
jcc-mirror scan  -data /data -pair media     # what does the publisher have?
jcc-mirror plan  -data /data -pair media     # what would a sync do? changes nothing
jcc-mirror sync  -data /data -pair media     # do it
jcc-mirror jobs  -data /data -pair media -state failed
```

A `sync` of a jcc pair ends with the lock gate on its database and a mirror pair
with the deletion phase; `-no-delete` leaves the second out, and `jcc-mirror db`
and `jcc-mirror delete` run either on its own.

Once that has been watched for a while, `jcc-mirror schedule` hands the same
three steps to the daemon; see **The schedule** below.

**The bootstrap.** The first ~30 TB comes across by USB, not down the wire — that
part is done by hand and jcc-mirror has no part in it. What it does have a part
in is recognising the result. Once the copy is on the Synology, `adopt` matches
the local tree against the manifest **on size alone** and records the matches as
already mirrored. Skipping it is expensive: the first sync would find an empty
`files` table, conclude it has nothing, and pull all 30 TB down the wire.

```bash
jcc-mirror scan  -data /data -pair media
jcc-mirror adopt -data /data -pair media
jcc-mirror plan  -data /data -pair media     # should now want almost nothing
```

Sizes, not mtimes: a USB copy that did not preserve timestamps would otherwise
make all 30 TB look changed. What `adopt` records as the local timestamp is the
publisher's, so the next diff agrees with the manifest. Run `plan` afterwards —
if it still wants tens of thousands of files, the adoption did not match and it
is better to find that out before the transfer starts.

### How it works, and what it refuses to do

**The scan** builds the manifest that does not exist on the other side: one
PROPFIND per directory, breadth-first, eight in flight. The frontier is the
manifest table itself rather than the walker's memory, so a walk interrupted
after ten minutes — a restart, a Ctrl-C, a transfer window closing —
resumes where it stopped instead of starting over. `Depth: infinity` is not used
even where a server honours it: on a tree this size it would return the whole
collection as one XML document.

A walk that stops early is `interrupted`, not `failed`, and it is only a
completed walk that sweeps the manifest rows it did not find. That asymmetry is
the deletion-safety rule of `DESIGN.md` §2.5: a share that has been unmounted on
the publisher's side answers every PROPFIND with an empty directory, and a scan
that finds no files at all is refused rather than believed.

**The diff** is a join between the manifest and local truth, on size and mtime,
with a two-second tolerance — WebDAV dates carry whole seconds and local ones
carry nanoseconds, and comparing them for equality would make every file look
changed on every scan, forever. The other two halves of that trap are handled in
the transfer: the downloaded file is stamped with the publisher's mtime *before*
the rename, and both sides of the comparison are NFC-normalized so a title with
an umlaut that passed through macOS does not look new.

**The transfer** takes one file at a time, in two to four ranged streams within
that file, into `<local>/.jccmirror/<sha256(relpath)>.part`. The name is a hash
of the path rather than something random, because a random name cannot be found
again after a restart — which is the whole of resume. Each round of chunks
advances a watermark in the `jobs` row, so an interrupted 40 GB file continues
within one round of where it stopped. The finished file is verified against the
size the manifest reported, timestamped, and `rename(2)`d into place from inside
the same directory tree, so it appears atomically or not at all.

Every file is a row in `jobs` with its own state machine and retry budget, which
is what makes a transfer retriable rather than an in-memory loop that dies with
the process. Anything a crash left `running` is requeued by the next run.

**The deletion** is a phase of its own, not an operation in the queue, and it
runs after a sync rather than inside one: the tree only ever shrinks once the
additions have landed. A pair whose queue still holds work, or holds a file that
failed for good, does not delete anything at all this time round.

It is opt-in per pair. `-mode additive` — the default — keeps everything; `-mode
mirror` quarantines what the publisher dropped. Four guards stand in front of it:

- **Two thresholds.** A deletion of more than the pair's `-guard` files, or of
  more than `delete.max_percent` of what the pair holds, is not carried out. It
  becomes one request per pair, which the setup view surfaces with a button and
  `jcc-mirror delete -approve` answers from a shell. Over the line, *nothing*
  goes — not the first N and then a stop.
- **An approval covers what was looked at.** It is bound to the walk it was
  computed from and to the count at that moment, so it can authorise a set that
  shrank since, never one that grew, and never one from a later walk. A scan in
  between makes it stale on purpose.
- **A quarantine, not an unlink.** A deleted file is renamed into
  `<local>/.jccmirror/trash/<date>/`, keeping its relative path — the same
  filesystem, so a 40 GB file costs a rename and no space. It stays there for
  `delete.retention` (a week by default), and `jcc-mirror trash` lists it, puts
  one back, or empties what has expired. A sync does that sweep on its own.
- **A non-empty manifest.** A pair whose manifest holds no files at all is a
  publisher who is not there, not one who deleted 30 TB, and is refused — the
  same rule the scanner applies to a walk that finds nothing.

```bash
jcc-mirror plan   -data /data -pair media          # says what would go, and what holds it
jcc-mirror delete -data /data -pair media          # runs the phase; may raise a request
jcc-mirror delete -data /data -pair media -approve # allows exactly that set, then runs
jcc-mirror trash  -data /data -pair media          # what is held, and until when
jcc-mirror trash  -data /data -pair media -restore "Filme/x.mkv"
```

A restored file is yours: no row of local truth is written for it, so the next
mirror run leaves it alone rather than quarantining it again.

### The jCC pair

`-type jcc` is the `ClipCornDB` directory, and it is an ordinary directory sync
plus two rules. Everything else about it — the covers, the walk, the diff, the
job queue — is the same machinery as a media pair.

**1 · Hard exclusions.** Refusals, not patterns, because no configuration makes
them correct. The walk drops them, so they never reach the manifest and nothing
downstream can queue them:

| | |
|---|---|
| `ClipCornUserData.db` · `ClipCornHistory.db` | Per-user. Copying the publisher's over destroys this side's own ratings, tags, filters and history. There are three databases in that directory, not two. |
| `*-journal` · `*-wal` · `*-shm` | Transient, and worse than useless apart from the exact database they belong to. |
| `*.~lock` | Belongs to whoever is running. |

**2 · A lock gate on the shared database.** `ClipCornDB.db` is the one file
jcc-mirror never puts through the job queue. It is copied as a phase of its own
at the end of a sync, in the order `DESIGN.md` §3 sets out:

```
1. HEAD  <remote>/ClipCornDB.db.~lock   → present ⇒ skip this cycle, note it
2. stat  <local>/ClipCornDB.db.~lock    → present ⇒ skip this cycle, note it
3. GET   the database → .ClipCornDB.db.incoming, staged in the target directory
4. verify its size against what the PROPFIND said
5. HEAD  the remote lock again          → appeared ⇒ discard, try later
6. stat  the local lock again           → appeared ⇒ discard, try later
7. keep the current database as a numbered backup
8. rename(.ClipCornDB.db.incoming → ClipCornDB.db)
```

The reasoning is one sentence long: jClipCorn writes the lock on open and deletes
it on a clean shutdown, so no lock means no writer and no hot journal, which is
what makes a plain byte copy of the `.db` file consistent. jcc-mirror never opens
the database and never reads a lock file's contents — the body is a PID from
another machine, and only its existence means anything.

Steps 5 and 6 are the non-obvious ones: user 1 can launch jClipCorn while the
copy runs, and one extra request each closes that window. Steps 3 and 8 stage in
the *target* directory, so the rename is same-filesystem and atomic — the local
exposure is one syscall rather than the length of a copy.

```bash
jcc-mirror pairs add -data /data -name clipcorn -type jcc \
    -remote "ClipCornDB" -local /mnt/clipcorn/ClipCornDB
jcc-mirror db -data /data -pair clipcorn            # both sides, both locks, the copies
jcc-mirror db -data /data -pair clipcorn -sync      # run the gate now
jcc-mirror db -data /data -pair clipcorn -rollback  # put the newest copy back
```

A held lock is not a failure: nothing is touched, the reason is recorded once
rather than once per attempt, and the scheduler comes back to it every ten
minutes until it clears. What will not clear on its own is a **stale** lock —
user 1's jClipCorn crashed and was never restarted — so one that has not moved
for `jcc.lock_stale` is reported as such, with `-force` (or the button on the
page) as the only way past it. It is never stolen automatically.

**Backups and the way back** (S5). Every replacement keeps a copy of the database
it displaced, in the data volume, with its hash and the publisher's timestamp;
`jcc.backups` — five by default — is how many are kept, at a few MB each. That is
the whole recovery story for a bad transfer, which once the gate has ruled out a
torn database is the only realistic failure left:

```bash
jcc-mirror db -data /data -pair clipcorn -rollback -backup 3
```

A rollback verifies the copy against its recorded size and hash before it moves
anything, keeps what it displaces so it is itself undoable, and needs neither the
publisher nor the tunnel — which is exactly when one is wanted. It does check
this side's lock. Afterwards the next sync compares the restored database against
the publisher's and fetches his again: what you want when the transfer was the
problem, and not when it was not — disable the pair first in that case.

A mirror pair never quarantines the database either. A walk that missed it is far
likelier than a collection that lost it.

**What is still not built**, so it does not come as a surprise:

| | |
|---|---|
| **An additive pair is still never shrunk.** | `additive` is the default and means what it says: files the publisher no longer has are counted by `plan` and kept. Deletion is opt-in per pair, with `-mode mirror`. |
| **No real dashboard.** | The setup view can now start and watch the operations, answer a held deletion, override a stale lock, roll a database back, edit the pairs and draw the schedule, which is enough to run the mirror without a shell. The six views, the SSE stream, the bandwidth charts and the notifications are still M6. |

## The schedule

One 7×24 grid, one cell per weekday-hour, each carrying **both** "may transfer"
and a bandwidth cap. Keeping them together is what makes "unlimited 02:00–08:00,
5 MB/s otherwise" a single setting rather than two that can disagree:

```bash
jcc-mirror schedule -data /data                                    # draw both grids
jcc-mirror schedule -data /data -set "* * = 5MiB; * 2-8 = full"    # the line above
jcc-mirror schedule -data /data -scan -set "* * = off; * 3-6 = on" # walk at night only
jcc-mirror schedule -data /data -automatic                         # run unattended
```

Rules are separated by `;`, and later ones win, so the first is the base and the
rest are the exceptions — the order a schedule is usually thought about in. Days
are `mon`…`sun`, `mon-fri`, `sat,sun` or `*`; hours are half-open spans that may
wrap over midnight (`22-2`), a single hour (`13`), or `*`; the cap is `off`,
`full`, or a rate (`5MiB`, `500k`, `5MiB/s`). An empty schedule is always open at
full speed, which is what an install that has never set one gets. The grid is
read in the configured timezone — `general.timezone`, deliberately not the
container's `TZ` — and `jcc-mirror schedule` draws it in that zone so a rule can
be checked against the week it actually produces.

Two things follow from the split between the grid and the switch:

- **The caps always apply.** A `sync` typed at a console is capped by the current
  cell too, because the cap is there to protect the link and the link does not
  care who pressed the button. `-limit 5MiB` overrides it, `-limit off` removes
  it.
- **The windows only gate the scheduler.** A hand-run command is an override by
  definition, so a closed window does not refuse one — it warns and runs uncapped.

`schedule.automatic` is off by default: updating the binary must not start
mirroring 30 TB on its own. With it on, the scheduler walks the enabled pairs in
priority order, scans one whose manifest is older than `schedule.scan_interval`,
then syncs one that has something to transfer, one run at a time. Crossing a
boundary mid-transfer is ordinary: a cap that changes is applied to the streams
already running, and a window that closes stops the run — the walk stays
resumable and every file keeps its watermark, so the next window carries on
rather than starting over. A run someone started by hand is never stopped by a
boundary.

**Free space** (S4) is checked before a plan starts and again before every file,
since a plan can run for days. A transfer whose completion would eat into
`transfer.reserve` — 50 GiB by default — is refused rather than run until the
volume is full, and `plan` says so first:

```
  free here      406.29 GiB, keeping 50.00 GiB in reserve
=> this does not fit: 3.60 TiB short. A sync would be refused before it started;
   narrow the pair with excludes, or lower the reserve.
```

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

The engine's settings live in the same table and appear in the same setup view,
because the key registry is what the form is generated from: the two grids, the
unattended switch and the scan interval under **Schedule**, walk concurrency and
the mtime tolerance under **Scan**, and streams per file, chunk size, attempts,
retry backoff, post-copy hashing and the free-space reserve under **Transfer**,
the deletion threshold and quarantine retention under **Deletion**, and the
database's name and directory, the stale-lock threshold and how many copies to
keep under **jCC**. The pairs themselves are the exception — they are rows of
their own, edited with `jcc-mirror pairs` until the dashboard grows an editor in
M6.

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

The engine tests are a whole mirror in a temporary directory: one tree standing
in for the publisher's share, one for the Synology, and the sqlite state between
them. The same thing works by hand, which is the fastest way to try something:

```bash
./jcc-mirror pairs add -data ./data -name demo -local "$PWD/dst" -remote ""
./jcc-mirror scan -data ./data -pair demo -remote-dir ./src
./jcc-mirror sync -data ./data -pair demo -remote-dir ./src
```

## Structure

```
main.go        command dispatch, the flags every command shares
static.go      the settings that are compiled in, and why
cmd_serve.go   the daemon
cmd_engine.go  what the mirror commands share: store, remote, engine
cmd_*.go       one file per command, mirror and diagnostic alike
app/           the running daemon: tunnel lifecycle, scheduler, dashboard,
               setup view
schedule/      the 7x24 grid: parsing, rendering, and when the answer next
               changes
engine/        the mirror: scan.go walks, diff.go plans, transfer.go moves
               bytes, adopt.go recognises the USB bootstrap, reap.go deletes
               behind the guards and trash.go is the quarantine, jcc.go is the
               jCC pair's refusals and lock gate, filter.go is the path
               globbing, limiter.go is the shared bandwidth cap and space.go
               the free-space preflight
store/         sqlite state: migrations, config with audit, events, and the
               engine's tables - pairs, manifest, scans, files, jobs, changes,
               delete_approvals, db_backups
wg/            wireguard-go + netstack: the tunnel, ping and device status
webdav/        PROPFIND, HEAD and ranged GET; propfind.go is the pure decoder
remote/        the three-method interface the engine talks to, plus the ranged
               read it needs on top of it
remote/localfs/  a local directory as a Remote, for tests and development
logs/ format/  the leveled logger and the number formatting everything shares
deploy/        Dockerfile and compose for running it on the Synology
```
