CREATE TABLE IF NOT EXISTS assets (
  id text PRIMARY KEY,
  namespace text NOT NULL,
  owner_service text NOT NULL,
  owner_type text NOT NULL,
  owner_id text NOT NULL,
  purpose text NOT NULL DEFAULT '',
  locale text NOT NULL DEFAULT '',
  original_file_name text NOT NULL DEFAULT '',
  object_key text NOT NULL UNIQUE,
  expected_mime_type text NOT NULL,
  detected_mime_type text NOT NULL DEFAULT '',
  size_bytes bigint NOT NULL DEFAULT 0,
  checksum_sha256 text NOT NULL DEFAULT '',
  etag text NOT NULL DEFAULT '',
  upload_status text NOT NULL,
  scan_status text NOT NULL,
  scan_details text NOT NULL DEFAULT '',
  processing_status text NOT NULL,
  visibility text NOT NULL,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  deleted_at timestamptz
);
CREATE INDEX IF NOT EXISTS assets_owner_idx ON assets(owner_service, owner_type, owner_id);
CREATE INDEX IF NOT EXISTS assets_public_idx ON assets(visibility, scan_status, processing_status) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS upload_sessions (
  id text PRIMARY KEY,
  asset_id text NOT NULL UNIQUE REFERENCES assets(id),
  idempotency_key text NOT NULL UNIQUE,
  max_size_bytes bigint NOT NULL,
  status text NOT NULL,
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL,
  completed_at timestamptz
);

CREATE TABLE IF NOT EXISTS asset_grants (
  id text PRIMARY KEY,
  asset_id text NOT NULL REFERENCES assets(id),
  subject_type text NOT NULL,
  subject_id text NOT NULL,
  permission text NOT NULL,
  idempotency_key text NOT NULL UNIQUE,
  expires_at timestamptz,
  created_at timestamptz NOT NULL,
  revoked_at timestamptz
);
CREATE INDEX IF NOT EXISTS asset_grants_lookup_idx ON asset_grants(asset_id, subject_type, subject_id, permission) WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS asset_scan_events (
  event_id text PRIMARY KEY,
  asset_id text NOT NULL REFERENCES assets(id),
  status text NOT NULL,
  details text NOT NULL DEFAULT '',
  etag text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL
);
