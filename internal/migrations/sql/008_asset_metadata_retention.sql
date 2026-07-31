CREATE INDEX IF NOT EXISTS assets_purged_retention_idx
  ON assets(purged_at, id)
  WHERE purged_at IS NOT NULL;
