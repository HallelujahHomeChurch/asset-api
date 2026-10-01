-- Source encode and CLI package validation share recording_processing_slots.
ALTER TABLE recording_sources
  ADD COLUMN processing_attempts integer NOT NULL DEFAULT 0 CHECK (processing_attempts BETWEEN 0 AND 3),
  ADD COLUMN claim_id text,
  ADD COLUMN claimed_until timestamptz,
  ADD COLUMN copy_attempt_id text,
  ADD COLUMN source_key text,
  ADD COLUMN source_etag text,
  ADD COLUMN source_verified_at timestamptz,
  ADD COLUMN package_id text UNIQUE REFERENCES recording_packages(id),
  ADD COLUMN processing_error text CHECK (processing_error IN ('invalid','retry','exhausted')),
  ADD COLUMN cleanup_after timestamptz NOT NULL DEFAULT now(),
  ADD CONSTRAINT recording_source_verified_metadata CHECK (
    (source_key IS NULL AND source_etag IS NULL AND source_verified_at IS NULL) OR
    (source_key IS NOT NULL AND source_etag IS NOT NULL AND source_verified_at IS NOT NULL AND copy_attempt_id IS NOT NULL)),
  ADD CONSTRAINT recording_source_processing_metadata CHECK (state NOT IN ('queued','processing','ready') OR source_verified_at IS NOT NULL),
  ADD CONSTRAINT recording_source_ready_package CHECK ((state='ready') = (package_id IS NOT NULL));

CREATE TABLE recording_source_attempts (
  claim_id text PRIMARY KEY CHECK (claim_id ~ '^[a-f0-9]{32}$'),
  source_id text NOT NULL REFERENCES recording_sources(id),
  state text NOT NULL CHECK (state IN ('processing','ready','failed','abandoned','purged')),
  created_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz
);
CREATE INDEX recording_source_attempts_cleanup_idx ON recording_source_attempts(created_at)
  WHERE state IN ('failed','abandoned');
