-- Optional derivatives never participate in capture validation or publication.
DROP INDEX recording_live_covers_auto;
CREATE UNIQUE INDEX recording_live_covers_auto ON recording_live_covers(scope) WHERE kind='auto' AND idempotency_key='auto';
ALTER TABLE recording_live_covers ADD COLUMN source_sequence integer CHECK(source_sequence BETWEEN 0 AND 4319);
CREATE TABLE recording_live_previews (
 capture_id text PRIMARY KEY REFERENCES recording_captures(id),
 completed_sequence integer NOT NULL DEFAULT -1 CHECK(completed_sequence BETWEEN -1 AND 4319),
 vtt text NOT NULL DEFAULT 'WEBVTT

' CHECK(octet_length(vtt)<=2097152),
 claim_id text,
 claimed_until timestamptz,
 next_attempt_at timestamptz NOT NULL DEFAULT now()
);
