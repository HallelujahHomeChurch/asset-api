ALTER TABLE recording_live DROP CONSTRAINT recording_live_revision_check;
ALTER TABLE recording_live ADD CONSTRAINT recording_live_revision_check CHECK (revision BETWEEN 0 AND 17281);
