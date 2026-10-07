-- Durable incremental validation and publication checkpoints. No media credentials.
CREATE TABLE recording_live (
 capture_id text PRIMARY KEY REFERENCES recording_captures(id),
 segments jsonb NOT NULL DEFAULT '[]' CHECK(jsonb_typeof(segments)='array' AND jsonb_array_length(segments)<=1440),
 revision bigint NOT NULL DEFAULT 0 CHECK(revision BETWEEN 0 AND 1441),
 published_revision bigint NOT NULL DEFAULT 0 CHECK(published_revision BETWEEN 0 AND revision),
 published_sequence integer NOT NULL DEFAULT -1 CHECK(published_sequence BETWEEN -1 AND 1439),
 published_media_end double precision NOT NULL DEFAULT 0 CHECK(published_media_end BETWEEN 0 AND 43200),
 last_advanced_at timestamptz,
 ended boolean NOT NULL DEFAULT false,
 ended_at timestamptz,
 claim_id text,
 claimed_until timestamptz,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts>=0),
 read_grant_until timestamptz,
 cleanup_after timestamptz NOT NULL DEFAULT now(),
 CHECK((claim_id IS NULL)=(claimed_until IS NULL))
);
CREATE TABLE recording_live_attempts (
 claim_id text PRIMARY KEY,
 capture_id text NOT NULL REFERENCES recording_captures(id),
 sequence integer NOT NULL CHECK(sequence BETWEEN 0 AND 1439),
 created_at timestamptz NOT NULL DEFAULT now(),
 finished_at timestamptz,
 purged boolean NOT NULL DEFAULT false
);
CREATE INDEX recording_live_attempts_cleanup ON recording_live_attempts(created_at) WHERE NOT purged;
INSERT INTO recording_live(capture_id) SELECT id FROM recording_captures;
