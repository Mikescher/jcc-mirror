-- The jCC pair (DESIGN.md §3, S5). Timestamps are unix milliseconds, as in 0001.

-- db_backups is the copy of ClipCornDB.db that the lock gate keeps before it
-- replaces the file. A few MB each, and the whole recovery story for a bad
-- transfer - which, once the gate has ruled out a torn database, is the only
-- realistic failure left.
--
-- path is absolute and lives in the data volume rather than beside the database:
-- the volume is what a rebuilt container keeps. hash is checked before a
-- rollback, because a backup that cannot be verified is not a backup.
CREATE TABLE db_backups (
    id       INTEGER PRIMARY KEY,
    pair_id  INTEGER NOT NULL REFERENCES pairs (id) ON DELETE CASCADE,
    ts       INTEGER NOT NULL,
    relpath  TEXT    NOT NULL,
    path     TEXT    NOT NULL,
    size     INTEGER NOT NULL,
    -- The mtime the copy carried, which is the publisher's. A rollback restores
    -- it along with the bytes, or the next diff would see a database that
    -- changed and fetch it all over again.
    mtime    INTEGER NOT NULL,
    hash     TEXT    NOT NULL,
    -- Why the copy was taken: 'replace' before the gate wrote a new database,
    -- 'rollback' before one was put back. The second is what makes a rollback
    -- itself undoable.
    reason   TEXT    NOT NULL CHECK (reason IN ('replace', 'rollback'))
) STRICT;

CREATE INDEX db_backups_pair ON db_backups (pair_id, ts DESC);
