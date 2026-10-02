-- Separate recording inputs. No ordinary assets, grants, or scan queue writes.
CREATE TABLE recording_sources (
  id text PRIMARY KEY CHECK (id ~ '^[a-f0-9]{32}$'),
  owner_service text NOT NULL CHECK (owner_service = 'hhc-web-api'),
  actor_id text NOT NULL CHECK (actor_id ~ '^[a-zA-Z0-9-]{1,80}$'),
  recording_id text NOT NULL CHECK (recording_id ~ '^[a-zA-Z0-9-]{1,80}$'),
  idempotency_key text NOT NULL UNIQUE CHECK (length(idempotency_key) BETWEEN 1 AND 128),
  file_name text NOT NULL CHECK (octet_length(file_name) BETWEEN 5 AND 255),
  size_bytes bigint NOT NULL CHECK (size_bytes > 0 AND size_bytes <= 50000000000),
  checksum_sha256 text NOT NULL CHECK (checksum_sha256 ~ '^[a-f0-9]{64}$'),
  block_count integer NOT NULL CHECK (block_count = (size_bytes + 16777215) / 16777216),
  state text NOT NULL CHECK (state IN ('uploading','finalizing','queued','processing','ready','failed','expired')),
  created_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL CHECK (expires_at = created_at + interval '24 hours'),
  completed_at timestamptz,
  retry_until timestamptz,
  staging_etag text,
  CHECK ((completed_at IS NULL AND retry_until IS NULL AND staging_etag IS NULL AND state IN ('uploading','expired')) OR
    (completed_at IS NOT NULL AND retry_until IS NOT NULL AND staging_etag IS NOT NULL AND length(staging_etag) > 0
      AND completed_at < expires_at AND retry_until = completed_at + interval '7 days' AND state <> 'uploading'))
);
CREATE UNIQUE INDEX recording_sources_recording_idx ON recording_sources(recording_id)
  WHERE state <> 'expired';
CREATE INDEX recording_sources_actor_idx ON recording_sources(actor_id, expires_at)
  WHERE state IN ('uploading','finalizing','queued','processing');
CREATE INDEX recording_sources_pending_idx ON recording_sources(completed_at)
  WHERE state = 'finalizing';
