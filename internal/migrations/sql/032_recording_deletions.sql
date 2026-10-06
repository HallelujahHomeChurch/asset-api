-- Durable owner tombstones prevent late upload creation after CMS deletion.
CREATE TABLE recording_deletions (
 recording_id text PRIMARY KEY CHECK (recording_id ~ '^[a-zA-Z0-9-]{1,80}$'),
 requested_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
