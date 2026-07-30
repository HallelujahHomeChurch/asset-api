ALTER TABLE assets
  ADD COLUMN IF NOT EXISTS processing_attempts integer NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS processing_next_attempt_at timestamptz;

DROP INDEX IF EXISTS assets_pending_processing_idx;
CREATE INDEX IF NOT EXISTS assets_pending_processing_idx
  ON assets(processing_next_attempt_at, updated_at)
  WHERE upload_status='completed'
    AND scan_status='clean'
    AND processing_status='pending'
    AND processing_attempts < 5
    AND deleted_at IS NULL
    AND purged_at IS NULL;

CREATE INDEX IF NOT EXISTS assets_expired_processing_idx
  ON assets(processing_claimed_until)
  WHERE upload_status='completed'
    AND scan_status='clean'
    AND processing_status='pending'
    AND processing_attempts >= 5
    AND deleted_at IS NULL
    AND purged_at IS NULL;

CREATE INDEX IF NOT EXISTS asset_grants_retention_idx
  ON asset_grants(asset_id);
CREATE INDEX IF NOT EXISTS asset_scan_events_retention_idx
  ON asset_scan_events(asset_id, created_at);

ALTER TABLE upload_sessions DROP CONSTRAINT IF EXISTS upload_sessions_asset_id_fkey;
ALTER TABLE upload_sessions
  ADD CONSTRAINT upload_sessions_asset_id_fkey
  FOREIGN KEY(asset_id) REFERENCES assets(id) ON DELETE CASCADE;

ALTER TABLE asset_grants DROP CONSTRAINT IF EXISTS asset_grants_asset_id_fkey;
ALTER TABLE asset_grants
  ADD CONSTRAINT asset_grants_asset_id_fkey
  FOREIGN KEY(asset_id) REFERENCES assets(id) ON DELETE CASCADE;

ALTER TABLE asset_scan_events DROP CONSTRAINT IF EXISTS asset_scan_events_asset_id_fkey;
ALTER TABLE asset_scan_events
  ADD CONSTRAINT asset_scan_events_asset_id_fkey
  FOREIGN KEY(asset_id) REFERENCES assets(id) ON DELETE CASCADE;
