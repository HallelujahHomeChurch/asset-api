-- Deliberately fail on existing duplicate recordings; do not discard files.
ALTER TABLE recording_uploads ADD CONSTRAINT recording_uploads_recording_unique UNIQUE (recording_id);
ALTER TABLE recording_uploads ADD COLUMN uploaded_at timestamptz;
-- No fabricated completion timestamps for legacy completed rows.
