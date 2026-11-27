CREATE TABLE recording_live_repairs (
 capture_id text NOT NULL REFERENCES recording_live(capture_id) ON DELETE CASCADE,
 sequence integer NOT NULL CHECK (sequence BETWEEN 1 AND 4319),
 rendition text NOT NULL CHECK (rendition IN ('480p','720p','1080p')),
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 claim_id text,
 PRIMARY KEY (capture_id,sequence,rendition)
);
CREATE INDEX recording_live_repairs_due ON recording_live_repairs(next_attempt_at);
