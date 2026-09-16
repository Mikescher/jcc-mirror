-- Empty is "whatever the process produces": its own uid:gid, 0644 and 0755 minus
-- the umask. Modes are octal text, e.g. '0000' for a Synology ACL share.
ALTER TABLE pairs ADD COLUMN owner     TEXT NOT NULL DEFAULT '';
ALTER TABLE pairs ADD COLUMN file_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE pairs ADD COLUMN dir_mode  TEXT NOT NULL DEFAULT '';
