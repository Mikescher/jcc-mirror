-- A notification target is one SCN account (and channel) with its own choice of
-- topics. The notify.* account settings and the notify.on_* toggles become rows
-- here; notify.tunnel_grace stays a setting, since it decides when a condition is
-- true and not who hears about it. Timestamps are unix milliseconds, as in 0001.
CREATE TABLE notify_targets (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,
    user_id    TEXT    NOT NULL,
    -- Plaintext, as it was in the config table. It is never handed out by the API.
    user_key   TEXT    NOT NULL DEFAULT '',
    channel    TEXT    NOT NULL DEFAULT '',
    sender     TEXT    NOT NULL DEFAULT '',
    enabled    INTEGER NOT NULL DEFAULT 1,
    -- Comma-separated topic keys, in the order of store.NotifyTopics.
    events     TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

-- The configured account becomes the first target. Each topic takes the stored
-- toggle, read the way strconv.ParseBool reads it, and its default otherwise.
INSERT INTO notify_targets (name, user_id, user_key, channel, sender, enabled, events, created_at, updated_at)
SELECT 'scn',
       trim(u.value),
       trim(k.value),
       coalesce((SELECT trim(value) FROM config WHERE key = 'notify.channel'), ''),
       coalesce(nullif((SELECT trim(value) FROM config WHERE key = 'notify.sender'), ''), 'jcc-mirror'),
       1,
       (SELECT coalesce(group_concat(t.topic, ',' ORDER BY t.ord), '')
        FROM (SELECT column1 AS topic, column2 AS ord, column3 AS def
              FROM (VALUES ('sync_failed', 1, 1), ('sync_ok', 2, 0), ('delete_blocked', 3, 1),
                           ('space_low', 4, 1), ('lock_stale', 5, 1), ('tunnel_down', 6, 1),
                           ('db_replaced', 7, 1), ('update', 8, 1))) t
        LEFT JOIN config c ON c.key = 'notify.on_' || t.topic
        WHERE CASE
                  WHEN c.value IS NULL THEN t.def
                  WHEN trim(c.value) IN ('1', 't', 'T', 'true', 'TRUE', 'True') THEN 1
                  WHEN trim(c.value) IN ('0', 'f', 'F', 'false', 'FALSE', 'False') THEN 0
                  ELSE t.def
              END = 1),
       CAST(strftime('%s', 'now') AS INTEGER) * 1000,
       CAST(strftime('%s', 'now') AS INTEGER) * 1000
FROM config u
JOIN config k ON k.key = 'notify.user_key'
WHERE u.key = 'notify.user_id' AND trim(u.value) <> '' AND trim(k.value) <> '';

DELETE FROM config
WHERE key IN ('notify.user_id', 'notify.user_key', 'notify.channel', 'notify.sender')
   OR key LIKE 'notify.on\_%' ESCAPE '\';

DELETE FROM config_audit
WHERE key IN ('notify.user_id', 'notify.user_key', 'notify.channel', 'notify.sender')
   OR key LIKE 'notify.on\_%' ESCAPE '\';
