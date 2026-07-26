ALTER TABLE assets
  ADD COLUMN IF NOT EXISTS purge_claimed_until timestamptz,
  ADD COLUMN IF NOT EXISTS purge_next_attempt_at timestamptz,
  ADD COLUMN IF NOT EXISTS purge_error text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS purged_at timestamptz;

CREATE INDEX IF NOT EXISTS assets_purge_idx
  ON assets(purge_next_attempt_at, updated_at)
  WHERE purged_at IS NULL;

ALTER TABLE assets
  ADD CONSTRAINT assets_visibility_check
  CHECK (visibility IN ('public','authenticated','restricted','private'));

ALTER TABLE asset_grants
  ADD CONSTRAINT asset_grants_subject_type_check
  CHECK (subject_type IN ('public','user','role','service','line_group','app_client')),
  ADD CONSTRAINT asset_grants_permission_check
  CHECK (permission IN ('read','delete'));
