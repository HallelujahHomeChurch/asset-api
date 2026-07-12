ALTER TABLE assets ADD COLUMN IF NOT EXISTS scan_attempts integer NOT NULL DEFAULT 0;
ALTER TABLE assets ADD COLUMN IF NOT EXISTS scan_next_attempt_at timestamptz;
ALTER TABLE assets ADD COLUMN IF NOT EXISTS scan_claimed_until timestamptz;

CREATE INDEX IF NOT EXISTS assets_pending_scan_idx
  ON assets(scan_next_attempt_at, created_at)
  WHERE upload_status = 'completed' AND scan_status = 'pending' AND deleted_at IS NULL;
