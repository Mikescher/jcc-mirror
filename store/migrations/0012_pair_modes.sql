-- foreign_keys: off
-- The quarantining mode is 'guarded' now, and 'mirror' deletes without guards.
-- Widening the CHECK means rebuilding the table; with foreign keys on, dropping it
-- would cascade into every table that references a pair.
CREATE TABLE pairs_new (
    id           INTEGER PRIMARY KEY,
    name         TEXT    NOT NULL UNIQUE,
    type         TEXT    NOT NULL CHECK (type IN ('raw', 'jcc')),
    remote_path  TEXT    NOT NULL,
    local_path   TEXT    NOT NULL,
    mode         TEXT    NOT NULL CHECK (mode IN ('mirror', 'guarded', 'additive')),
    includes     TEXT    NOT NULL DEFAULT '[]',
    excludes     TEXT    NOT NULL DEFAULT '[]',
    delete_guard INTEGER NOT NULL DEFAULT 100,
    priority     INTEGER NOT NULL DEFAULT 100,
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    remote_id    INTEGER REFERENCES remotes (id) ON DELETE RESTRICT,
    owner        TEXT    NOT NULL DEFAULT '',
    file_mode    TEXT    NOT NULL DEFAULT '',
    dir_mode     TEXT    NOT NULL DEFAULT ''
) STRICT;

INSERT INTO pairs_new (id, name, type, remote_path, local_path, mode, includes, excludes,
                       delete_guard, priority, enabled, created_at, updated_at, remote_id,
                       owner, file_mode, dir_mode)
SELECT id, name, type, remote_path, local_path,
       CASE mode WHEN 'mirror' THEN 'guarded' ELSE mode END,
       includes, excludes, delete_guard, priority, enabled, created_at, updated_at, remote_id,
       owner, file_mode, dir_mode
FROM pairs;

DROP TABLE pairs;
ALTER TABLE pairs_new RENAME TO pairs;

CREATE INDEX pairs_remote ON pairs (remote_id);
