-- Timestamps are unix milliseconds throughout: a scan and a transfer both produce
-- several events a second, and whole seconds lose their order.

CREATE TABLE config (
    key        TEXT PRIMARY KEY,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE config_audit (
    id        INTEGER PRIMARY KEY,
    ts        INTEGER NOT NULL,
    key       TEXT    NOT NULL,
    old_value TEXT,
    new_value TEXT    NOT NULL,
    actor     TEXT    NOT NULL
) STRICT;

CREATE INDEX config_audit_key_ts ON config_audit (key, ts DESC);

-- Populated in M2. It exists here because events reference it, and a foreign key
-- added later would mean rewriting the events table.
CREATE TABLE pairs (
    id           INTEGER PRIMARY KEY,
    name         TEXT    NOT NULL UNIQUE,
    type         TEXT    NOT NULL CHECK (type IN ('raw', 'jcc')),
    remote_path  TEXT    NOT NULL,
    local_path   TEXT    NOT NULL,
    mode         TEXT    NOT NULL CHECK (mode IN ('mirror', 'additive')),
    includes     TEXT    NOT NULL DEFAULT '[]',
    excludes     TEXT    NOT NULL DEFAULT '[]',
    delete_guard INTEGER NOT NULL DEFAULT 100,
    priority     INTEGER NOT NULL DEFAULT 100,
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE events (
    id      INTEGER PRIMARY KEY,
    ts      INTEGER NOT NULL,
    level   TEXT    NOT NULL CHECK (level IN ('debug', 'info', 'warn', 'error')),
    kind    TEXT    NOT NULL,
    pair_id INTEGER REFERENCES pairs (id) ON DELETE SET NULL,
    message TEXT    NOT NULL,
    data    TEXT
) STRICT;

CREATE INDEX events_ts ON events (ts DESC);
CREATE INDEX events_kind_ts ON events (kind, ts DESC);
