CREATE TABLE IF NOT EXISTS asset_derivative_outbox (
  event_id text PRIMARY KEY CHECK (event_id <> ''),
  asset_id text NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
  asset_etag text NOT NULL CHECK (asset_etag <> ''),
  attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  available_at timestamptz NOT NULL,
  claimed_until timestamptz,
  delivered_at timestamptz,
  last_error text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL,
  UNIQUE(asset_id, asset_etag)
);

CREATE INDEX IF NOT EXISTS asset_derivative_outbox_pending_idx
  ON asset_derivative_outbox(available_at, created_at)
  WHERE delivered_at IS NULL;

INSERT INTO asset_derivative_outbox(event_id, asset_id, asset_etag, available_at, created_at)
SELECT 'backfill-derivative-' || md5(id || ':' || etag), id, etag, updated_at, updated_at
FROM assets
WHERE upload_status = 'completed'
  AND scan_status = 'clean'
  AND processing_status = 'pending'
  AND detected_mime_type IN ('image/jpeg', 'image/png', 'image/webp')
  AND deleted_at IS NULL
  AND purged_at IS NULL
  AND etag <> ''
ON CONFLICT(asset_id, asset_etag) DO NOTHING;
