CREATE TABLE recording_uploads (
  id text PRIMARY KEY,
  asset_version_id text NOT NULL UNIQUE,
  owner_service text NOT NULL CHECK (owner_service = 'hhc-web-api'),
  admin_id text NOT NULL,
  recording_id text NOT NULL,
  file_name text NOT NULL,
  object_key text NOT NULL UNIQUE,
  upload_id text NOT NULL,
  idempotency_key text NOT NULL UNIQUE,
  status text NOT NULL CHECK (status IN ('created','completing','validating','ready','failed','cancelled','deleting','deleted')),
  size_bytes bigint NOT NULL CHECK (size_bytes > 0 AND size_bytes <= 10000000000),
  checksum_sha256 text NOT NULL CHECK (checksum_sha256 ~ '^[0-9a-f]{64}$'),
  created_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
  validation_attempts integer NOT NULL DEFAULT 0 CHECK (validation_attempts >= 0),
  validation_claim_id text,
  validation_claimed_until timestamptz,
  duration_seconds double precision CHECK (duration_seconds > 0),
  validation_error text
);

CREATE INDEX recording_uploads_admin_status_idx ON recording_uploads(admin_id, status, expires_at);
CREATE INDEX recording_uploads_recording_status_idx ON recording_uploads(recording_id, status);
CREATE INDEX recording_uploads_validation_idx ON recording_uploads(status, validation_claimed_until) WHERE status = 'validating';
