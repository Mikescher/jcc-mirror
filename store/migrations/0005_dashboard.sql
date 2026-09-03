-- The dashboard's own tables (DESIGN.md §4, §4.1, §7). Timestamps are unix
-- milliseconds, as in 0001.

-- bw_samples is the Bandwidth view's time series. A minute row is written by the
-- daemon from the counters on the WebDAV transport; rolled up to an hour after a
-- week and to a day after a quarter, because a per-minute series kept forever is
-- half a million rows a year and nobody reads minutes from last spring.
--
-- span is part of the key rather than a separate table so a rollup is one INSERT
-- ... SELECT and one DELETE over the same shape.
CREATE TABLE bw_samples (
    span      TEXT    NOT NULL CHECK (span IN ('minute', 'hour', 'day')),
    -- The start of the bucket, truncated to the span in UTC. The weekday and
    -- hour a heatmap draws are worked out in the configured timezone by whoever
    -- reads these rows, not here.
    ts        INTEGER NOT NULL,
    bytes_in  INTEGER NOT NULL DEFAULT 0,
    bytes_out INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (span, ts)
) STRICT;

-- notify_state is what keeps a persistent condition from notifying on every
-- tick. A stale source lock or a dead tunnel is true for hours, and SCN's daily
-- quota is finite, so the message goes out on the edge - once when the condition
-- starts and once when it clears (DESIGN.md §4.1).
--
-- It is a table rather than a field on the App because a restart must not
-- re-announce a condition that was already reported; a container that crash-loops
-- would otherwise spend the quota by itself.
CREATE TABLE notify_state (
    kind        TEXT    NOT NULL,
    -- 0 for a condition that belongs to no particular pair. A real id is not a
    -- foreign key: the row has to outlive the pair, or removing a pair while one
    -- of its conditions is active would lose the edge.
    pair_id     INTEGER NOT NULL DEFAULT 0,
    active      INTEGER NOT NULL DEFAULT 0,
    since       INTEGER NOT NULL,
    notified_at INTEGER NOT NULL DEFAULT 0,
    detail      TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (kind, pair_id)
) STRICT;
