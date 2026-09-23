CREATE TABLE IF NOT EXISTS asset_account_cleanup_operations (
  idempotency_key text PRIMARY KEY,
  subject_ref text NOT NULL CHECK (subject_ref ~ '^[0-9a-f]{64}$'),
  status text NOT NULL CHECK (status IN ('pending','completed')),
  affected_count bigint NOT NULL DEFAULT 0 CHECK (affected_count >= 0),
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS asset_account_cleanup_subject_idx
  ON asset_account_cleanup_operations(subject_ref, updated_at);
