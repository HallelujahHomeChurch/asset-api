ALTER TABLE upload_sessions
  ADD COLUMN IF NOT EXISTS staging_object_key text NOT NULL DEFAULT '';
