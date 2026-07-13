ALTER TABLE assets ADD COLUMN IF NOT EXISTS processing_error text NOT NULL DEFAULT '';
ALTER TABLE assets ADD COLUMN IF NOT EXISTS processing_claimed_until timestamptz;

CREATE TABLE IF NOT EXISTS asset_derivatives (
  asset_id text NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
  variant text NOT NULL CHECK (variant IN ('small','medium','large')),
  object_key text NOT NULL UNIQUE,
  mime_type text NOT NULL,
  width integer NOT NULL CHECK (width > 0),
  height integer NOT NULL CHECK (height > 0),
  size_bytes bigint NOT NULL CHECK (size_bytes > 0),
  etag text NOT NULL,
  created_at timestamptz NOT NULL,
  PRIMARY KEY(asset_id,variant)
);

CREATE INDEX IF NOT EXISTS assets_pending_processing_idx
  ON assets(updated_at)
  WHERE upload_status='completed' AND scan_status='clean' AND processing_status='pending' AND deleted_at IS NULL;
