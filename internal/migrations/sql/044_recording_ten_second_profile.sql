-- Explicit opt-in only; existing captures retain the original 900-frame profile.
ALTER TABLE recording_captures ADD COLUMN segment_frames integer NOT NULL DEFAULT 900 CHECK (segment_frames IN (300,900));
ALTER TABLE recording_captures DROP CONSTRAINT recording_captures_declared_objects_check;
ALTER TABLE recording_captures ADD CONSTRAINT recording_captures_declared_objects_check CHECK (declared_objects BETWEEN 0 AND 13000);
ALTER TABLE recording_live DROP CONSTRAINT recording_live_segments_check;
ALTER TABLE recording_live ADD CONSTRAINT recording_live_segments_check CHECK (jsonb_typeof(segments)='array' AND jsonb_array_length(segments)<=4320);
ALTER TABLE recording_live DROP CONSTRAINT recording_live_revision_check;
ALTER TABLE recording_live ADD CONSTRAINT recording_live_revision_check CHECK (revision BETWEEN 0 AND 4321);
ALTER TABLE recording_live DROP CONSTRAINT recording_live_published_sequence_check;
ALTER TABLE recording_live ADD CONSTRAINT recording_live_published_sequence_check CHECK (published_sequence BETWEEN -1 AND 4319);
ALTER TABLE recording_live_attempts DROP CONSTRAINT recording_live_attempts_sequence_check;
ALTER TABLE recording_live_attempts ADD CONSTRAINT recording_live_attempts_sequence_check CHECK (sequence BETWEEN 0 AND 4319);
