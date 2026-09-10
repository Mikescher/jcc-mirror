-- The dashboard has no bearer token. It is reachable on the LAN and inside the
-- WireGuard tunnel and nowhere else, so the token bought nothing but a lookup in
-- a container log before every change (DESIGN.md §4).

-- Nothing reads the key any more, but GET /api/config walks the stored rows, and
-- a value with no definition behind it is a setting the dashboard cannot draw.
DELETE FROM config WHERE key = 'dashboard.token';

-- The trail is per key, so the token's rows go without touching the history of
-- anything else. What they record is the mask rather than the value - the point
-- is only that the trail stops naming a setting that no longer exists.
DELETE FROM config_audit WHERE key = 'dashboard.token';
