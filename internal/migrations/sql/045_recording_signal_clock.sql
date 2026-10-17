ALTER TABLE recording_captures ADD COLUMN last_media_confirmed_at timestamptz;
ALTER TABLE recording_captures ADD COLUMN idle_end_sequence_exclusive integer CHECK (idle_end_sequence_exclusive BETWEEN 1 AND 4320);
