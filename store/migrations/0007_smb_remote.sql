-- The publisher's share is reached over SMB rather than WebDAV (DESIGN.md §2.1),
-- so the single setting that named it by URL gives way to remote.host,
-- remote.share and remote.path. Timestamps are unix milliseconds, as in 0001.

-- The old URL is not thrown away in silence. It held the publisher's address and
-- the directory the mirror was rooted at, and both have to be entered again in
-- their new form, so it is copied into the event log before it goes. The event
-- log is the trail chosen over leaving the row in place because it is the one an
-- operator is shown without going to look for it - and because Config() already
-- hides a key that is no longer in the registry, which would leave the row as a
-- value that silently never takes effect.
INSERT INTO events (ts, level, kind, message)
SELECT CAST(strftime('%s', 'now') AS INTEGER) * 1000,
       'warn',
       'config.changed',
       'the remote is now read over SMB; the previous WebDAV base URL was ' || value ||
       ' - fill in remote.host, remote.share and remote.path to match it'
FROM config
WHERE key = 'remote.url' AND trim(value) <> '';

DELETE FROM config WHERE key = 'remote.url';
