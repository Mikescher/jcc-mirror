-- The updater fetches its binary over plain http(s) and nothing else, so there
-- is no remote for it to read with (DESIGN.md §5).

-- GET /api/config walks the stored rows, and a value with no definition behind
-- it is a setting the dashboard cannot draw.
DELETE FROM config WHERE key = 'update.remote';
DELETE FROM config_audit WHERE key = 'update.remote';

-- A path on the publisher's share is not a URL the updater can fetch. Cleared,
-- updating is off until someone enters one, rather than failing on every check.
DELETE FROM config
 WHERE key = 'update.url'
   AND lower(trim(value)) NOT LIKE 'http://%'
   AND lower(trim(value)) NOT LIKE 'https://%';
