-- Existing consumers still require ready_at + 30 days until explicitly activated.
CREATE TABLE recording_retention_policy (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  retention_days integer NOT NULL DEFAULT 30 CHECK (retention_days BETWEEN 1 AND 365),
  revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
  activated_at timestamptz
);
INSERT INTO recording_retention_policy(singleton) VALUES (true);

CREATE TABLE recording_retention_previews (
  id text PRIMARY KEY,
  actor_id text NOT NULL,
  retention_days integer NOT NULL CHECK (retention_days BETWEEN 1 AND 365),
  revision bigint NOT NULL,
  evaluated_at timestamptz NOT NULL,
  affected_count bigint NOT NULL,
  affected_bytes bigint NOT NULL,
  idempotency_key text,
  result jsonb,
  UNIQUE(actor_id, idempotency_key)
);

ALTER TABLE recording_packages
  DROP CONSTRAINT recording_package_ready_metadata,
  ADD CONSTRAINT recording_package_ready_metadata CHECK
    (state <> 'ready' OR (final_prefix IS NOT NULL AND ready_at IS NOT NULL AND media_expires_at IS NOT NULL)),
  ADD COLUMN retention_revision bigint NOT NULL DEFAULT 1,
  ADD COLUMN grant_cleanup_after timestamptz;
