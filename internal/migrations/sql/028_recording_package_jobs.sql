-- Additive HLS processing leases. The two database slots bound concurrent
-- work across overlapping scheduled executions, not merely within a replica.
ALTER TABLE recording_packages
  ADD COLUMN validation_attempts integer NOT NULL DEFAULT 0 CHECK (validation_attempts BETWEEN 0 AND 3),
  ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT now(),
  ADD COLUMN claim_id text,
  ADD COLUMN claimed_until timestamptz,
  ADD COLUMN final_prefix text,
  ADD COLUMN ready_at timestamptz,
  ADD COLUMN media_expires_at timestamptz,
  ADD COLUMN cleanup_after timestamptz NOT NULL DEFAULT now(),
  ADD COLUMN validation_error text CHECK (validation_error IN ('invalid','retry','exhausted')),
  ADD CONSTRAINT recording_package_ready_metadata CHECK
    (state <> 'ready' OR (final_prefix IS NOT NULL AND ready_at IS NOT NULL AND media_expires_at = ready_at + interval '30 days'));

CREATE TABLE recording_processing_slots (
  slot integer PRIMARY KEY CHECK (slot IN (1,2)),
  job_id text,
  claim_id text UNIQUE,
  leased_until timestamptz,
  CHECK ((job_id IS NULL AND claim_id IS NULL AND leased_until IS NULL) OR
         (job_id IS NOT NULL AND claim_id IS NOT NULL AND leased_until IS NOT NULL))
);
INSERT INTO recording_processing_slots(slot) VALUES (1),(2);

-- Keep each server-only final prefix discoverable after crash/reclaim.
CREATE TABLE recording_package_attempts (
  claim_id text PRIMARY KEY CHECK (claim_id ~ '^[a-zA-Z0-9-]{1,80}$'),
  package_id text NOT NULL REFERENCES recording_packages(id),
  state text NOT NULL CHECK (state IN ('processing','ready','failed','abandoned','purged')),
  created_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz
);
CREATE INDEX recording_package_attempts_cleanup_idx ON recording_package_attempts(created_at)
  WHERE state IN ('failed','abandoned');
