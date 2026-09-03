-- Deletion and its guards (DESIGN.md §2.5). Timestamps are unix milliseconds, as
-- in 0001.

-- delete_approvals is a deletion that was too large to run unattended. A plan
-- over the pair's threshold is neither executed nor thrown away: it becomes a row
-- here, which the dashboard surfaces for one approval and the next run consumes.
--
-- scan_id binds the request to the walk it was computed from, and files bounds it
-- from the other side. An approval may therefore authorise a set that shrank
-- between the look and the act, never one that grew, and never one from a later
-- walk that nobody looked at.
CREATE TABLE delete_approvals (
    id         INTEGER PRIMARY KEY,
    pair_id    INTEGER NOT NULL REFERENCES pairs (id) ON DELETE CASCADE,
    scan_id    INTEGER NOT NULL,
    files      INTEGER NOT NULL,
    bytes      INTEGER NOT NULL,
    reason     TEXT    NOT NULL,
    state      TEXT    NOT NULL CHECK (state IN ('pending', 'approved', 'used', 'rejected')),
    created_at INTEGER NOT NULL,
    decided_at INTEGER,
    decided_by TEXT,
    used_at    INTEGER
) STRICT;

-- One open request per pair. A second scan updates the first rather than stacking
-- a queue of stale numbers nobody can tell apart.
CREATE UNIQUE INDEX delete_approvals_open ON delete_approvals (pair_id) WHERE state IN ('pending', 'approved');
CREATE INDEX delete_approvals_pair ON delete_approvals (pair_id, id DESC);
