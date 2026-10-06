-- Optional derived media. Defaults also enqueue existing ready packages.
ALTER TABLE recording_packages
  ADD COLUMN preview_state text NOT NULL DEFAULT 'pending' CHECK (preview_state IN ('pending','processing','ready','failed')),
  ADD COLUMN preview_attempts integer NOT NULL DEFAULT 0 CHECK (preview_attempts BETWEEN 0 AND 3),
  ADD COLUMN preview_claim_id text,
  ADD COLUMN preview_claimed_until timestamptz,
  ADD COLUMN preview_next_attempt_at timestamptz NOT NULL DEFAULT now();
CREATE TABLE recording_preview_attempts (
  claim_id text PRIMARY KEY CHECK (claim_id ~ '^[a-zA-Z0-9-]{1,80}$'),
  package_id text NOT NULL REFERENCES recording_packages(id)
);
CREATE INDEX recording_preview_pending_idx ON recording_packages(preview_next_attempt_at)
  WHERE state='ready' AND preview_state IN ('pending','processing');
