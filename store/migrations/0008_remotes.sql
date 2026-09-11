-- A remote is one SMB share on the publisher's side, and there can be any number
-- of them: the six remote.* settings become rows here, and every pair names the
-- remote it reads from. Timestamps are unix milliseconds, as in 0001.
CREATE TABLE remotes (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,
    host       TEXT    NOT NULL,
    share      TEXT    NOT NULL,
    path       TEXT    NOT NULL DEFAULT '',
    username   TEXT    NOT NULL DEFAULT '',
    -- Plaintext, as it was in the config table: a read-only account reached over
    -- a private tunnel. It is never handed out by the API.
    password   TEXT    NOT NULL DEFAULT '',
    domain     TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

-- NULL is a pair with no remote yet. RESTRICT rather than CASCADE or SET NULL:
-- removing a remote must not silently take a pair's manifest with it, nor leave
-- the pair pointing at nothing.
ALTER TABLE pairs ADD COLUMN remote_id INTEGER REFERENCES remotes (id) ON DELETE RESTRICT;

CREATE INDEX pairs_remote ON pairs (remote_id);

-- The single remote that was configured becomes the first row, named after its
-- share, and every existing pair reads from it - which is what they did before.
INSERT INTO remotes (name, host, share, path, username, password, domain, created_at, updated_at)
SELECT trim(s.value),
       trim(h.value),
       trim(s.value),
       coalesce((SELECT value FROM config WHERE key = 'remote.path'), ''),
       coalesce((SELECT value FROM config WHERE key = 'remote.user'), ''),
       coalesce((SELECT value FROM config WHERE key = 'remote.password'), ''),
       coalesce((SELECT value FROM config WHERE key = 'remote.domain'), ''),
       CAST(strftime('%s', 'now') AS INTEGER) * 1000,
       CAST(strftime('%s', 'now') AS INTEGER) * 1000
FROM config h
JOIN config s ON s.key = 'remote.share'
WHERE h.key = 'remote.host' AND trim(h.value) <> '' AND trim(s.value) <> '';

UPDATE pairs SET remote_id = (SELECT min(id) FROM remotes);

-- A half-configured remote cannot become a row, so what there was of it goes to
-- the event log rather than nowhere. The password is left out of the message.
INSERT INTO events (ts, level, kind, message)
SELECT CAST(strftime('%s', 'now') AS INTEGER) * 1000,
       'warn',
       'config.changed',
       'remotes are now records of their own, and the stored remote was incomplete (host ' ||
       quote(coalesce((SELECT value FROM config WHERE key = 'remote.host'), '')) || ', share ' ||
       quote(coalesce((SELECT value FROM config WHERE key = 'remote.share'), '')) ||
       ') - add it again on the Remotes tab'
WHERE NOT EXISTS (SELECT 1 FROM remotes)
  AND EXISTS (SELECT 1 FROM config
              WHERE key IN ('remote.host', 'remote.share', 'remote.path', 'remote.user', 'remote.domain')
                AND trim(value) <> '');

DELETE FROM config
WHERE key IN ('remote.host', 'remote.share', 'remote.path', 'remote.user', 'remote.password', 'remote.domain');
