-- Older workers ignore this additive checkpoint. NULL includes previously purged
-- attempts whose last successful sweep is unknown.
ALTER TABLE recording_live_attempts ADD COLUMN last_swept_at timestamptz;
CREATE INDEX recording_live_attempts_sweep_order
 ON recording_live_attempts(capture_id,purged,last_swept_at NULLS FIRST,created_at,claim_id);
