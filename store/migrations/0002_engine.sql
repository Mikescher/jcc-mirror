-- The scanner, the differ and the transfer engine (DESIGN.md §2.3, §2.4, §7).
-- Timestamps are unix milliseconds, as in 0001.

-- scans records one walk of a pair's remote root. A walk of the real collection
-- runs for minutes, so it has to survive a restart or a window boundary: the row
-- stays 'running' and the manifest remembers which directories it already listed.
CREATE TABLE scans (
    id          INTEGER PRIMARY KEY,
    pair_id     INTEGER NOT NULL REFERENCES pairs (id) ON DELETE CASCADE,
    started_at  INTEGER NOT NULL,
    finished_at INTEGER,
    dirs        INTEGER NOT NULL DEFAULT 0,
    files       INTEGER NOT NULL DEFAULT 0,
    bytes       INTEGER NOT NULL DEFAULT 0,
    -- 'interrupted' is a walk that stopped and can be picked up again: a closed
    -- transfer window, a restart, a directory that would not list. 'failed' is one
    -- that must not be: it was superseded, or it found nothing at all.
    state       TEXT    NOT NULL CHECK (state IN ('running', 'interrupted', 'done', 'failed')),
    error       TEXT
) STRICT;

CREATE INDEX scans_pair_started ON scans (pair_id, started_at DESC);

-- manifest is the last remote walk: what the publisher has. There is no manifest
-- on his side and never will be, so this table is the only answer to "what is
-- over there" that jcc-mirror ever gets.
--
-- scan_id is the walk that last touched the row. It drives the resume - a
-- directory of the current scan with listed = 0 has not been visited yet - and
-- the sweep that drops what the walk no longer found. It carries no foreign key
-- on purpose: the manifest has to outlive the scans rows, which are pruned.
CREATE TABLE manifest (
    pair_id INTEGER NOT NULL REFERENCES pairs (id) ON DELETE CASCADE,
    relpath TEXT    NOT NULL,
    size    INTEGER NOT NULL,
    mtime   INTEGER NOT NULL,
    is_dir  INTEGER NOT NULL,
    listed  INTEGER NOT NULL DEFAULT 0,
    scan_id INTEGER NOT NULL,
    seen_at INTEGER NOT NULL,
    PRIMARY KEY (pair_id, relpath)
) STRICT;

CREATE INDEX manifest_pending ON manifest (pair_id, scan_id) WHERE is_dir = 1 AND listed = 0;

-- files is local truth: what jcc-mirror believes lies under the pair's local
-- root, and the mtime it stamped on it - which is the publisher's, never the
-- download time. The diff is a join between this table and manifest, so both
-- sides store relpath NFC-normalized or every title with an umlaut looks new
-- (DESIGN.md §2.3).
CREATE TABLE files (
    pair_id     INTEGER NOT NULL REFERENCES pairs (id) ON DELETE CASCADE,
    relpath     TEXT    NOT NULL,
    size        INTEGER NOT NULL,
    mtime       INTEGER NOT NULL,
    hash        TEXT,
    -- 'adopted' rows were matched by size alone during the USB bootstrap and were
    -- never transferred by us; a future scrub needs to be able to tell.
    state       TEXT    NOT NULL CHECK (state IN ('synced', 'adopted')),
    verified_at INTEGER NOT NULL,
    PRIMARY KEY (pair_id, relpath)
) STRICT;

-- jobs is the transfer queue and the retry policy in one table. A crash, a
-- self-update or a closed transfer window leaves rows in 'running'; they are
-- requeued at the next start. That, rather than an in-memory retry loop that
-- dies with the process, is what makes a transfer retriable (DESIGN.md §2.4).
--
-- bytes_done is the resume watermark: the contiguous prefix of the .part file
-- that is known good.
CREATE TABLE jobs (
    id              INTEGER PRIMARY KEY,
    pair_id         INTEGER NOT NULL REFERENCES pairs (id) ON DELETE CASCADE,
    relpath         TEXT    NOT NULL,
    -- 'delete' is accepted here from the start: adding a value to a CHECK later
    -- means rebuilding the table. M2 never queues one - deletion is M4.
    op              TEXT    NOT NULL CHECK (op IN ('add', 'replace', 'delete')),
    bytes_total     INTEGER NOT NULL,
    bytes_done      INTEGER NOT NULL DEFAULT 0,
    mtime           INTEGER NOT NULL,
    state           TEXT    NOT NULL CHECK (state IN ('pending', 'running', 'verifying', 'done', 'failed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL DEFAULT 0,
    error           TEXT,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
) STRICT;

-- One open job per file. Two scans in a row must not queue the same transfer
-- twice; the second one updates the first.
CREATE UNIQUE INDEX jobs_open ON jobs (pair_id, relpath) WHERE state IN ('pending', 'running', 'verifying');
CREATE INDEX jobs_queue ON jobs (pair_id, state, next_attempt_at);

-- changes is the per-file history the dashboard's Changes view lists. A run that
-- transfers ten thousand files writes ten thousand rows here and one event: that
-- split is what keeps the event log readable (DESIGN.md §4).
CREATE TABLE changes (
    id          INTEGER PRIMARY KEY,
    ts          INTEGER NOT NULL,
    pair_id     INTEGER REFERENCES pairs (id) ON DELETE SET NULL,
    relpath     TEXT    NOT NULL,
    op          TEXT    NOT NULL CHECK (op IN ('add', 'replace', 'delete', 'fail')),
    size_before INTEGER,
    size_after  INTEGER,
    error       TEXT
) STRICT;

CREATE INDEX changes_ts ON changes (ts DESC);
CREATE INDEX changes_pair_ts ON changes (pair_id, ts DESC);
