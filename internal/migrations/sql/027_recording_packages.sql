-- Additive producer contract. Existing Blob assets and MP4 recording uploads
-- are untouched; consumers remain gated until the HLS validator is released.
CREATE TABLE recording_packages (
  id text PRIMARY KEY CHECK (id ~ '^[a-zA-Z0-9-]{1,80}$'),
  session_id text NOT NULL UNIQUE CHECK (session_id = id),
  owner_service text NOT NULL CHECK (owner_service = 'hhc-web-api'),
  actor_id text NOT NULL CHECK (actor_id ~ '^[a-zA-Z0-9-]{1,80}$'),
  recording_id text NOT NULL CHECK (recording_id ~ '^[a-zA-Z0-9-]{1,80}$'),
  idempotency_key text NOT NULL UNIQUE CHECK (length(idempotency_key) BETWEEN 1 AND 128),
  state text NOT NULL CHECK (state IN ('uploading','freezing','validating','ready','failed','expired')),
  size_bytes bigint NOT NULL CHECK (size_bytes > 0 AND size_bytes <= 10000000000),
  inventory jsonb NOT NULL CHECK (jsonb_typeof(inventory) = 'object'),
  created_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL CHECK (expires_at > created_at AND expires_at <= created_at + interval '24 hours'),
  completed_at timestamptz,
  CHECK ((state = 'uploading' AND completed_at IS NULL) OR state IN ('failed','expired') OR completed_at IS NOT NULL)
);

CREATE UNIQUE INDEX recording_packages_recording_idx ON recording_packages(recording_id);
CREATE INDEX recording_packages_actor_active_idx ON recording_packages(actor_id, expires_at)
  WHERE state IN ('uploading','freezing','validating');
CREATE INDEX recording_packages_pending_idx ON recording_packages(created_at)
  WHERE state IN ('freezing','validating');
