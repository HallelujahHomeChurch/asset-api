CREATE TABLE IF NOT EXISTS asset_scan_outbox (
  event_id text PRIMARY KEY CHECK (event_id <> ''),
  asset_id text NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
  asset_etag text NOT NULL CHECK (asset_etag <> ''),
  attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  available_at timestamptz NOT NULL,
  claimed_until timestamptz,
  delivered_at timestamptz,
  last_error text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS asset_scan_outbox_pending_idx
  ON asset_scan_outbox(available_at, created_at)
  WHERE delivered_at IS NULL;

INSERT INTO asset_scan_outbox(event_id, asset_id, asset_etag, available_at, created_at)
SELECT 'backfill-' || id, id, etag, updated_at, updated_at
FROM assets
WHERE upload_status = 'completed' AND scan_status = 'pending'
  AND deleted_at IS NULL AND purged_at IS NULL AND etag <> ''
ON CONFLICT(event_id) DO NOTHING;
