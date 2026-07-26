ALTER TABLE upload_sessions
  ADD COLUMN IF NOT EXISTS caller_service text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS operation text NOT NULL DEFAULT 'create_upload',
  ADD COLUMN IF NOT EXISTS request_fingerprint text NOT NULL DEFAULT '';

ALTER TABLE upload_sessions DROP CONSTRAINT IF EXISTS upload_sessions_idempotency_key_key;
CREATE UNIQUE INDEX IF NOT EXISTS upload_sessions_idempotency_scope_idx
  ON upload_sessions(caller_service, operation, idempotency_key);

ALTER TABLE asset_grants
  ADD COLUMN IF NOT EXISTS caller_service text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS operation text NOT NULL DEFAULT 'create_grant',
  ADD COLUMN IF NOT EXISTS request_fingerprint text NOT NULL DEFAULT '';

ALTER TABLE asset_grants DROP CONSTRAINT IF EXISTS asset_grants_idempotency_key_key;
CREATE UNIQUE INDEX IF NOT EXISTS asset_grants_idempotency_scope_idx
  ON asset_grants(caller_service, operation, idempotency_key);
