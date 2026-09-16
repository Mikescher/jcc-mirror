# jcc-mirror

One-way replication of a jClipCorn collection from the publisher's NAS to the
subscriber's Synology. See `DESIGN.md` for the design; this README covers what is
built so far.

**Status: M7, complete.** It mirrors, on a schedule, at a rate you choose; it
can shrink as well as grow; it handles the jClipCorn database itself; there is a
dashboard to watch all of it from; and it now replaces its own binary. On top of
M1's skeleton — sqlite state, a config table with an audit trail, the `Remote`
interface with an SMB2 implementation and a local fake, an event log and
`/healthz` — M2's engine — a resumable walk that builds the manifest the
publisher does not have, a differ, a transfer with bounded reads at an offset,
`.part` files, verification, an atomic rename and a per-file job queue with
retries, plus adopt mode for the USB bootstrap — M3's scheduler — one 7×24 grid
that says both when bytes may move and how fast, a shared limiter that a window
boundary adjusts mid-transfer, and a free-space preflight — M4's deletion behind
every guard of `DESIGN.md` §2.5 — and M5's jCC pair — hard exclusions that never
copy the per-user databases, a lock gate on the shared one that is re-checked
after the copy, and a numbered backup of every database it replaces — there is
now the **dashboard**: an Angular build compiled into the binary, four live views and the settings pages fed
by one Server-Sent Events stream, a per-minute bandwidth series that rolls
itself up rather than growing without bound, a read-only mode that one button in
the header lifts, and push notifications over SCN coalesced hard enough to live
inside a daily quota. And on top of all of it, M7's **self-update**: a binary
fetched over plain HTTP(S), checked, smoke-run and
re-exec'd in place, with a supervisor that puts the old one back if the new one
will not start.

A pair is additive until it is told otherwise, which is the point: run
additive-only until the diff is trusted.

## Running it

```bash
cd deploy && docker compose up -d && docker compose logs -f
```

Nothing has to be known before the process starts. The LAN dashboard does not
depend on the tunnel, so a first boot against an empty database answers on
`:8080` with the dashboard and empty settings pages:

1. Open `http://<synology>:8080` and press **Unlock**. Do not publish that port
   past the LAN: the dashboard has no authentication of its own.
2. On **Connection**, paste the client config the rootserver generated
   for this container — `[Interface]` private key and all — into the importer. It
   fills every tunnel setting in one go: the private key, the rootserver's public
   key and endpoint, this container's tunnel address, the allowed IPs, DNS, the
   MTU and the keepalive. Directives a userspace tunnel has no host side to act
   on — `ListenPort`, `PostUp`, `Table` — are skipped and named back to you.
   Every one of those fields can be typed instead; the importer only saves the
   typing.
3. Save. The tunnel comes up in place — no restart — and the same dashboard also
   starts answering on `http://<our-tunnel-address>:8080`, which is how the
   publisher reaches it with no port forward.
4. On **Remotes**, add the publisher's share: his address inside the tunnel, the
   share, the directory inside it the remote is rooted at, and a read-only
   account on his DSM. One remote per share; add a second for a second share.
   SMB has no usable anonymous mode, so the account is required even for a share
   that is open to everyone. Each pair then picks the remote it reads from on
   the **Pairs** tab.
5. **Test the remote** proves the whole path end to end, one remote at a time.
   Its answer lands in the event log.

A private key is made up at first start so the tunnel has an identity before
anyone has configured one, and the container log prints the public half of
whatever is stored. It is worth a glance after an import: it has to match the
peer entry the rootserver holds for this container.

Every endpoint is open, so a remote can be added from `curl` as easily as from
the page:

```bash
curl -H 'Content-Type: application/json' \
     -d '{"name":"media","host":"10.13.13.2","share":"media","user":"ro","password":"…"}' \
     http://<synology>:8080/api/remotes
```

There is no credential anywhere in this, which cuts both ways: nothing can be
stolen out of the browser or forged in it, and nothing stands between a request
and the daemon either. What scopes the dashboard is the network — the compose
file maps `:8080` onto the LAN, and the other listener is inside the tunnel — so
whoever can reach it is already on the inside. The Unlock button is a guard rail
in the browser and nothing the daemon knows about: it is remembered in
`localStorage`, it defaults to locked, and what it stops is a stray click, not a
request.

| Endpoint | |
|---|---|
| `GET /` and every route under it | The dashboard. Unknown paths answer with the shell, because the router is on the client |
| `GET /healthz` | Status as JSON; 503 only when the database has stopped answering |
| `GET /api/stream` | Server-Sent Events: `state` frames with the status and the running operation, plus `events` and `changes` as they happen, and `log` frames carrying the container's own log |
| `GET /api/status` · `/api/schedule` · `/api/config` · `/api/config/audit` · `/api/events` · `/api/changes` · `/api/remotes` · `/api/pairs` · `/api/jobs` · `/api/scans` · `/api/trash` · `/api/runs` · `/api/bandwidth` · `/api/diagnostics` · `/api/update` · `/api/remote/list` (optional `remote`) | Read views. Secrets are never returned — a remote says whether it has a password and nothing more; `status` carries the current window and cap, and `remotes` |
| `POST /api/config` · `/api/remote/probe` (optional `remote`) | A settings save, and one listing of a remote's root to prove the path end to end |
| `POST /api/config/wireguard/import` (`config`) | A whole wg-quick file, spread over the tunnel settings through that same save; answers with what it changed and what it had to skip |
| `POST /api/remotes` · `/api/remotes/update` · `/api/remotes/delete` | JSON or a form, with `id` naming the remote on the last two. A blank password keeps the stored one and `clearPassword` clears it; a delete answers 409 while a pair still reads from the remote |
| `POST /api/pairs` · `/api/pairs/update` · `/api/pairs/delete` | JSON or a form; a JSON body may carry one key and changes only that |
| `POST /api/pairs/deletions` (`id`, `decision`) | The one click a held deletion waits for |
| `POST /api/pairs/database/rollback` (`id`, optional `backup`) | Puts a kept copy of a jcc pair's database back; answers when it is done |
| `POST /api/trash/restore` (`id`, `path`, optional `day`) | Takes one file back out of the quarantine |
| `POST /api/runs` (`kind`, `pair`, optional `force`) | Answers as soon as the run has started, never when it has finished. `force` is the lock-gate override |
| `POST /api/runs/cancel` | Stopping is routine: a walk stays resumable and every transfer keeps its watermark |
| `GET /api/update` | The self-updater's panel: what is running, what the update URL has, what the last update did |
| `POST /api/update/check` · `/api/update/apply` (optional `force`) · `/api/update/rollback` | Apply and rollback answer as soon as the binary is in place; the daemon then re-execs, so the next request reaches the new process at the same address |
| `POST /api/diagnostics/ping` · `/api/diagnostics/throughput` (optional `remote`) | The M0 measurements, from the page rather than a shell |
| `GET /api/notify/topics` · `/api/notify/targets` | The topics a target can choose from, with their defaults, and the targets. A target says whether it has a user key and nothing more |
| `POST /api/notify/targets` · `/api/notify/targets/update` · `/api/notify/targets/delete` | JSON or a form, with `id` naming the target on the last two. `events` is the full list of topics; a blank `userKey` keeps the stored one |
| `POST /api/notify/test` (`id`) | Sends one message to that target, ignoring its topics and its switch, and answers with what SCN said |

`remote`, where an endpoint takes one, is a remote's id; left out, it is the
first remote.

## The mirror

Two ways in, on the same state: the dashboard and the CLI.

**From the dashboard.** The **Now** view lists the pairs with how far behind each
one is and a Plan / Dry run / Scan / Adopt / Sync / Delete / Database button beside each —
plus, when a deletion is over a threshold, the two buttons that answer it, and for
a jcc pair the copies of its database with a rollback beside each one. A run
happens in the background and the page follows it over the event stream, so a
sync that takes a day and a half is watchable from a phone.
One runs at a time — the transfer engine moves one file at a time by design, and
the scheduler walks the pairs in priority order for the same reason. Stop is
always safe: a walk stays resumable and every transfer keeps its place in the
file.

This is the route to use if the container's shell is awkward to reach, which on a
Synology it usually is.

**From the CLI.** Every command takes `-data`, so they all work on the same sqlite
state the daemon runs on, and `-remote-dir` points the mirror at a local
directory instead of the pair's remote — the whole milestone can be exercised
with no NAS and no tunnel. SMB has no in-process server the way HTTP does, which
is why that flag exists at all: without it nothing here could be run end to end
except against a real share, and it is what the tests run on.

A remote is one share over there, and a pair is one directory of a remote mapped
onto one directory here. Nothing is mirrored until both exist:

```bash
jcc-mirror remotes add -data /data -name media \
    -host 10.13.13.2 -share media -user ro -pass '…'
jcc-mirror pairs add -data /data -name media -from media \
    -remote "Filme" -local /mnt/media/Filme -mode additive \
    -exclude "**/*.tmp,Serien/Trash/**"
jcc-mirror pairs -data /data
```

With one remote stored, `-from` can be left off and the pair reads from that one;
with several, `pairs add` asks which. `pairs set <pair> -from <remote>` moves a
pair to another remote, `remotes set` changes only the flags it is given — an
explicit `-pass ""` clears the password — and `remotes rm` refuses a remote while
a pair still reads from it. The mirror commands read through the pair's own
remote; `-from` on any of them reads through another for that one run.

Then the three steps, in order. They are separate commands because each answers
a different question, and because the first weeks are meant to be run by hand:

```bash
jcc-mirror scan  -data /data -pair media     # what does the publisher have?
jcc-mirror plan  -data /data -pair media     # what would a sync do? changes nothing
                                             # (-scan walks first: scan and plan in one)
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
listing per directory — a CREATE and a QUERY_DIRECTORY, one round trip —
breadth-first, eight in flight, multiplexed over the one session. The frontier is
the manifest table itself rather than the walker's memory, so a walk interrupted
after ten minutes — a restart, a Ctrl-C, a transfer window closing — resumes
where it stopped instead of starting over.

A walk that stops early is `interrupted`, not `failed`, and it is only a
completed walk that sweeps the manifest rows it did not find. That asymmetry is
the deletion-safety rule of `DESIGN.md` §2.5: a share that has been unmounted on
the publisher's side answers every listing with an empty directory, and a scan
that finds no files at all is refused rather than believed.

**The diff** is a join between the manifest and local truth, on size and mtime,
with a two-second tolerance — SMB answers with a FILETIME good to 100 ns, the
manifest keeps whole milliseconds and the destination volume rounds to whatever
it rounds to, and comparing the three for equality would make every file look
changed on every scan, forever. The other two halves of that trap are handled in
the transfer: the downloaded file is stamped with the publisher's mtime *before*
the rename, and both sides of the comparison are NFC-normalized so a title with
an umlaut that passed through macOS does not look new.

**The transfer** takes one file at a time, in two to four bounded reads at
different offsets within it, into `<local>/.jccmirror/<sha256(relpath)>.part`.
The name is a hash of the path rather than something random, because a random
name cannot be found again after a restart — which is the whole of resume. Each
round of chunks advances a watermark in the `jobs` row, so an interrupted 40 GB
file continues within one round of where it stopped. The finished file is
verified against the size the manifest reported, timestamped, and `rename(2)`d
into place from inside the same directory tree, so it appears atomically or not
at all.

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
  becomes one request per pair, which the Now view surfaces with a button and
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
1. probe <remote>/ClipCornDB.db.~lock   → present ⇒ skip this cycle, note it
2. stat  <local>/ClipCornDB.db.~lock    → present ⇒ skip this cycle, note it
3. read  the database → .ClipCornDB.db.incoming, staged in the target directory
4. verify its size against what the remote's stat reported
5. probe the remote lock again          → appeared ⇒ discard, try later
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
copy runs, and one extra existence probe each side closes that window. Steps 3
and 8 stage in the *target* directory, so the rename is same-filesystem and
atomic — the local exposure is one syscall rather than the length of a copy.

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

## The dashboard

Angular, built into `web/dist` and compiled into the binary with `go:embed`, so
the deployment stays one file and one volume. Every page is fed by a single
Server-Sent Events stream on `/api/stream` — one way, reconnects for free, and it
survives whatever proxy is in front of it. The frames it carries are the same rows
that were written to the `events` and `changes` tables, so the live view and the
history view share a schema rather than agreeing by accident.

| View | |
|---|---|
| **Now** | What is running: pair, file, both progress bars, instantaneous and average rate, ETA, queue depth, the active window and its cap. Under it, every pair with its buttons, a held deletion with the two that answer it, and the recent runs. |
| **Changes** | Per-file history — added, replaced, deleted, failed — filterable by pair and operation. It is the only place one file's fate is recorded. |
| **Events** | Scans and syncs with their durations, the database replaced or skipped for a lock, deletion guards, tunnel edges, configuration changes, errors. Each row can be expanded into the JSON it carries. |
| **Bandwidth** | The per-minute series drawn as inline SVG, plus the 7×24 heatmap that makes the weekday/weekend shape visible. Minutes are rolled up to hours after a week and to days after a quarter — a per-minute series kept forever is half a million rows a year. |
| **Connection** | The tunnel settings and the wg-quick importer, with the tunnel's live state: endpoint, our addresses, listen port, and each peer's handshake age and counters. |
| **Remotes** | The publisher's shares, each with its reachability and a Test button, plus RTT through the tunnel, a throughput probe and a directory-by-directory explorer of any remote's tree. |
| **Schedule** · **Transfer** · **jCC** | Their settings groups; Transfer also shows how many names the walk had to normalize to NFC. |
| **Pairs** | The pairs, each with the free space of its volume and an "explain plan" button. |
| **Notifications** | The SCN targets, the tunnel grace period, and the conditions currently raised. |
| **Update** | The self-update settings and panel: what runs, what the URL has, install, install anyway, roll back. |
| **System** | Retention and timezone, the live log tail and the data directory. |
| **Audit** | Every configuration change. |

The settings pages sit in the same tab bar as the four live views, each with its own draft and its own save bar at the foot of the viewport. The fields are generated from the key registry, so a key added later costs no HTML; a page-to-group table is the one place a settings group is told which page it belongs on, and a group no page claims lands on **System** rather than disappearing.

Every view is drawn from an open endpoint, the log tail — on
`GET /api/diagnostics` and as `log` frames on the stream — included; the Unlock
button is a signal in the browser and nothing the daemon is told about. The
bandwidth series is sampled off the connection the SMB session runs over rather
than off the transfer engine, so what the graph shows is what the link actually
carried — SMB's own framing and the walk's thousands of directory listings
included.

## Notifications

The dashboard is pull-only, so without a push channel a stale source lock or a
dead tunnel is invisible until someone goes and looks. **SCN**
(`simplecloudnotifier.de`) closes that. Add a target under **Notifications** in
Config — an SCN user id and user key, optionally a channel and a sender name, and
the topics that target wants to hear about. There can be as many targets as there
are people or channels to tell, each with its own topics and an on/off switch;
with none, nothing is ever sent.

Two properties of that API shape the whole thing. A `403` means the daily quota is
exhausted, so notifications are a finite resource and jcc-mirror must never emit
one per file: everything is coalesced to at most one message per *sync run* or per
*state transition*. And `msg_id` is an idempotency key, so it is a hash of
`(kind, pair, day, target)` rather than sent-state tracked here — a retry after a network
blip is free, and an alert that recurs all day collapses on the server side.

| Topic | Default for a new target | Priority | |
|---|---|---|---|
| Sync run failed, or finished with failures | on | 1 | One message per run: "412 files, 2 failed", never two |
| Deletion guard tripped, awaiting approval | on | 2 | On the edge, and again when it is answered |
| Free space below the reserve | on | 2 | On the edge; only a sync can raise or clear it, because only a sync looks |
| Source lock stale past the threshold | on | 1 | On the edge. Until someone overrides it the database silently never syncs |
| Tunnel down longer than the grace period | on | 1 | On the edge, after `notify.tunnel_grace` — a rootserver rebooting should not wake anyone |
| `ClipCornDB.db` replaced | on | 0 | |
| Self-update applied, or rolled back | on | 0 / 2 | One topic, two priorities: an update is news, an update that had to be undone is something to look at |
| Sync run finished cleanly | off | 0 | A mirror that works is not news |

"On the edge" is the discipline the quota forces: a condition that is true for
hours is announced when it starts and once more when it clears, and never in
between. The edge is decided once, for all targets together. The state that decides it lives in sqlite rather than in memory, so a
container that crash-loops does not spend the day's allowance re-announcing the
same thing. A notification that fails is written to `events` and never fails the
run that produced it — it is telemetry, not a step.

## Self-update

"Update in place without restarting docker" is not literally possible — the
kernel holds the running executable's inode — but re-execing gives exactly the
property that was wanted: same PID, same container, nothing for the orchestration
to notice. The new binary is published on an ordinary web server and fetched
from there directly, over the host's own network — not through the tunnel, and
with no remote involved.

Point it at one on the **Update** page. `update.url` takes an `http://` or
`https://` URL and nothing else; empty switches updating off. A Nextcloud public
share works as it is, through its WebDAV address:

```
update.url       https://cloud.example.com/public.php/dav/files/<share-token>/jcc-mirror
update.auto      false
update.interval  6h
```

The server has to answer a `HEAD` with a `Last-Modified`; one that does not is an
error rather than "nothing newer". If it wants credentials, put them in the URL
(`https://user:password@host/...`) and they are sent as Basic auth. The URL is
shown with the password masked in the update panel, the event log, the log and
`update.json`.

There is no version file and no manifest. The question is literally "is the file
over there newer than mine", and its modification time answers it. What it is
compared against is the later of this binary's compiled-in build stamp and the
timestamp of whatever the last update installed — the second half matters,
because a binary built at 10:00 and uploaded at 10:05 would otherwise read as
newer than itself, forever.

Then, in order:

1. `HEAD` the binary and read its `Last-Modified`. Newer ⇒ there is an update.
2. Download it into `/data/bin/jcc-mirror.new`, and check it is plausible before
   trusting it: the `Content-Length` the server reported, and the ELF magic in the first
   four bytes.
3. Run it once as `jcc-mirror.new --version` and require a sane answer. That is
   the check the magic number cannot do: a binary for the other architecture is a
   perfectly well-formed ELF that will not run here.
4. Keep the current binary as `jcc-mirror.prev` and rename the new one into
   place.
5. Stop whatever is transferring — after the install rather than before, so a
   sync is not interrupted for an update that turns out not to be installable.
   Nothing is lost either way: a walk stays resumable and every transfer keeps
   its watermark, which is what makes "just stop" an acceptable quiesce.
6. Shut the listeners down and `exec` it.

None of this is signing, and it is not meant to be. The binary comes from a
server you control — over TLS with an `https` URL — so what these checks are aimed
at is corruption: a download cut short, the wrong architecture, an HTML error
page saved as a binary. Those are the failures that actually happen.

**The safety net.** The container's command is `jcc-mirror supervise serve` — the
supervisor is a subcommand of ours rather than a shell script, because the
runtime image is distroless and has no shell. It runs the daemon as a child and
watches for one thing: an update that has not yet proved itself exiting non-zero.
When that happens it puts `jcc-mirror.prev` back — or, if the update replaced the
image's binary and there is no previous one, removes the managed binary so the
image's runs again — and starts that instead. The restored daemon says what
happened, in the event log and as a notification, and **the binary that was rolled
back is not installed again on its own**. That last part is what keeps a binary
that crashes on startup from being fetched, installed, rolled back and fetched
again on every check.

An update proves itself by staying up for a minute. After that a crash is an
ordinary crash and Docker's restart policy handles it, rather than the updater
hiding it behind a rollback.

From a shell, on the same state:

```bash
jcc-mirror update -data /data                    # what is running and what is installed
jcc-mirror update -data /data -check             # and what the update URL has
jcc-mirror update -data /data -apply             # install it; restart to run it
jcc-mirror update -data /data -apply -force      # install it even if it is not newer
jcc-mirror update -data /data -rollback          # step back off one, without the dashboard
```

`-rollback` needs neither the daemon, the tunnel nor the network, which is the
point of it: it is for the case where the thing that is broken is the binary that
would otherwise answer the button. `-check` and `-apply` fetch the update URL
directly, exactly as the daemon does.

Publishing a new one is an upload to wherever `update.url` points. `make release`
builds the amd64 binary, pushes the image and `PUT`s the binary to the WebDAV
address in the Makefile's `RELEASE_DAV`:

```bash
make release
```

The binary directory is in the data volume, since the image's `/usr/local/bin` is
read-only and the container is unprivileged:

```
/data/bin/jcc-mirror        in use; absent means the image's is
/data/bin/jcc-mirror.prev   the one it replaced
/data/bin/update.json       what the last update did, which the supervisor reads
```

## The M0 diagnostics

The spike commands are kept: each answers a question the design rests on, and
they are the diagnostics when something is wrong. `-data /data` takes every
setting left unset from the same sqlite the daemon runs on — the share they open
is the first remote, or the one `-from` names by name or id — so they need no
flags once the dashboard is configured.

| # | Question | Command | Why it could sink the design |
|---|---|---|---|
| 1 | Does the rootserver forward between its spokes? | `ping` | Both NASes are clients of the same WireGuard server. A server set up as an internet gateway will not route between them, and nothing else here can work until it does. |
| 2 | Does the tunnel come up from inside an unprivileged container? | `status` | The whole no-`NET_ADMIN` deployment rests on wireguard-go plus netstack behaving on the Synology. |
| 3 | Does one directory listing off DSM's share cost what we assume? | `ls` | How long a single listing takes, multiplied by the number of directories, is what a walk costs; and how the names come back, since a tree that is partly NFD makes every affected title look new on every scan. |
| 4 | How long does a full metadata walk take? | `walk` | There is no remote manifest, so this is what every scan costs, forever. It sets the scan schedule. |
| 5 | Does DSM hold up under a long read at an offset? | `get`, `resume`, `soak` | Resume is the property the transfer engine is built on. If reading from an offset is unreliable, a 40 GB file interrupted at 39 GB starts again from zero. |

```bash
docker compose run --rm jcc-mirror ping     -data /data -target <publisher-tunnel-addr> -count 10
docker compose run --rm jcc-mirror status   -data /data
docker compose run --rm jcc-mirror ls       -data /data -path "" -limit 50
docker compose run --rm jcc-mirror ls       -data /data -from archive -path ""
docker compose run --rm jcc-mirror walk     -data /data -workers 8 -out /data/manifest.ndjson
docker compose run --rm jcc-mirror resume   -data /data -path "Filme/Some.Big.File.mkv" -length $((256<<20))
docker compose run --rm jcc-mirror soak     -data /data -path "Filme/Some.Big.File.mkv" -duration 4h
```

Every setting can still be given as a flag instead, and a flag always wins over
the stored value — trying something other than what is configured is the point.
The remote is `-host`, `-share`, `-share-path`, `-user`, `-pass` and `-domain`,
and each of them wins over the stored remote field by field;
`-no-tunnel` talks to `-host` out of the host network rather than through
WireGuard, and `-remote-dir` needs no server at all — `ls` and `walk` run against
a local directory with the same code and the same output. Run `jcc-mirror help`
for the full list.

### Reading the results

- **`ping` gets no replies but the handshake succeeds.** The tunnel is fine and
  the spoke-to-spoke route is not. Check `net.ipv4.ip_forward=1` on the
  rootserver and that its peer entries' `AllowedIPs` cover *both* clients. This
  is not a jcc-mirror bug and it is the most likely way the setup fails.
- **`walk` reports non-NFC names.** Part of the tree passed through macOS. The
  diff must normalize, or every title with an umlaut looks new on every scan.
- **`resume` reports differing hashes.** Reading from an offset is not usable
  against this server, and the transfer design needs rethinking before M2. This is
  the single most consequential result the spike can produce.
- **`soak` reports a first error at a suspiciously round time.** DSM drops long
  transfers on a timer. Not fatal — the job state machine has to treat a dropped
  connection as routine — but it decides how aggressive the retry policy is.

## Configuration

There is no config file, no `.env`, and nothing is read from the environment.
Only true invariants are compiled in (`static.go`): the data directory and the
two listen addresses. Everything else lives in the `config` table, is edited in
the dashboard, and every change is recorded in `config_audit` — with secrets
masked, so the trail says that a password changed and never what it changed to.

One setting is generated at first start rather than typed: the WireGuard private
key, so the tunnel has an identity before anyone has given it one. It is only a
stand-in — the rootserver issues the key its peer entry knows, and importing that
config overwrites it — but either way the key exists in exactly one place and
never passes through a compose file, a shell history or `ps`.

The engine's settings live in the same table and appear in the same form, because
the key registry is what the settings pages are generated from — a setting added in a
later milestone costs no HTML. The tunnel's nine settings are under **Tunnel**;
the two grids, the unattended switch and the scan interval are under
**Schedule**; walk concurrency and the mtime tolerance under **Scan**; streams
per file, chunk size, attempts, retry backoff, post-copy hashing and the
free-space reserve under **Transfer**; the deletion threshold and quarantine
retention under **Deletion**; the database's name and directory, the stale-lock
threshold and how many copies to keep under **jCC**; the tunnel-down grace under
**Notifications**; the `http(s)`
URL the binary comes from, whether to install it unattended and how often to look
under **Update**; and how long the event log and the per-file history are kept under
**Retention**, a year each by default, swept once an hour. The remotes, the
pairs and the notification targets are the exception — they are rows of their
own, one remote per share on the publisher's side with its host, share, path in
the share, account and NTLM domain, edited on the **Remotes** and **Pairs** tabs
or with `jcc-mirror remotes` and `jcc-mirror pairs`, and one target per SCN
account with its topics, edited on the **Notifications** tab.

## Publisher-side setup

Nothing of ours runs there. Turn on **File Services → SMB** in DSM, share the
directories, and create a read-only account for them — SMB has no usable
anonymous mode, so there has to be one even for a share everybody may read. Port
445 as it comes, with none of the transport encryption DSM offers: the tunnel
already encrypts and authenticates every packet, and doing it twice only costs
the NAS's CPU.

## Development

```bash
make test        # unit tests plus end-to-end runs against a local directory
make vet
make build
make web         # rebuild the dashboard into web/dist after changing web/src
./jcc-mirror serve -data ./data -lan 127.0.0.1:8080
```

`web/dist` is checked in, which is what keeps `make build`, `make syno` and the
Dockerfile free of a node toolchain: a Go build never needs one. `make web` is
what regenerates it, and it has to be run before committing a change to the
dashboard or the binary still carries the old one.

For working on the dashboard itself, point the daemon at a running `ng serve`
instead of the embedded build. One port then answers both halves, so there is no
CORS and no second origin — the API, the event stream and the UI all come from
`:8080`. `localhost` rather than `127.0.0.1` in the target: `ng serve` binds to
the name, which resolves to `::1`.

```bash
cd web && npx ng serve &                                   # :4200, rebuilds on save
./jcc-mirror serve -data ./data -lan 127.0.0.1:8080 -dev-ui http://localhost:4200
```

Nothing a plain `go test ./...` runs needs a network, a tunnel or a NAS. There is
no in-process SMB server to stand a share up with, so what everything runs against
is `remote/localfs`, which serves a directory as a `remote.Remote` directly — with
whole-second timestamps, so a test cannot pass on fidelity the real remote does
not have. Holding the two implementations to the same answers therefore takes a
real server, and that check is opt-in: `TestAgreesWithTheFake`, in
`smb/client_integration_test.go`, skips unless it is told where one is, and it is
the reason `remote.Remote` is an interface at all.

```bash
JCC_SMB_HOST=10.13.13.2 JCC_SMB_SHARE=media JCC_SMB_USER=ro JCC_SMB_PASS=… \
JCC_SMB_LOCAL_DIR=/volume1/media go test ./smb/
```

`JCC_SMB_LOCAL_DIR` is that server's own directory: the fixture is written on the
local side of the share, so the two remotes are asked about the same tree rather
than about two that ought to agree.

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
cmd_serve.go   the daemon, and the re-exec a self-update ends in
cmd_engine.go  what the mirror commands share: store, remote, engine
cmd_*.go       one file per command, mirror and diagnostic alike
app/           the running daemon: tunnel lifecycle, scheduler, the dashboard's
               API, the SSE stream, the bandwidth meter and rollups, the
               notification policy, and the embedded UI with its dev proxy. What
               it reads the publisher with is an interface, so Options.RemoteDir
               can put a local directory where the remotes are
schedule/      the 7x24 grid: parsing, rendering, and when the answer next
               changes
engine/        the mirror: scan.go walks, diff.go plans, transfer.go moves
               bytes, adopt.go recognises the USB bootstrap, reap.go deletes
               behind the guards and trash.go is the quarantine, jcc.go is the
               jCC pair's refusals and lock gate, filter.go is the path
               globbing, limiter.go is the shared bandwidth cap and space.go
               the free-space preflight
store/         sqlite state: migrations, config with audit, events, and the
               engine's tables - remotes, pairs, manifest, scans, files, jobs, changes,
               delete_approvals, db_backups - plus the dashboard's bw_samples
               and the notifications' notify_targets and notify_state
wg/            wireguard-go + netstack: the tunnel, ping and device status, and
               quick.go, which reads the config file the server generated
smb/           the SMB2 client: one long-lived session, dialed over a net.Conn
               the caller hands it, which is what puts it inside the tunnel
remote/        the three-method interface the engine talks to, plus the two
               things it needs on top of it - a bounded read at an offset, and an
               existence probe for the lock gate
remote/localfs/  a local directory as a Remote, for tests, development and
               -remote-dir
notify/        the SimpleCloudNotifier client; the coalescing above it is app/
update/        the self-updater: the HTTP(S) source, the binary directory in
               the data volume, and the supervisor that undoes a bad update
web/           the Angular dashboard: src/ is the source, dist/ is the checked-in
               build the binary embeds
logs/ format/  the leveled logger - which also keeps the last few hundred lines
               for the dashboard's tail - and the number formatting everything
               shares
deploy/        Dockerfile and compose for running it on the Synology
```
