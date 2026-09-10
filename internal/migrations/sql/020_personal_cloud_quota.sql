CREATE TABLE personal_cloud_quotas (
  owner_user_id text PRIMARY KEY,
  quota_bytes bigint NOT NULL CHECK (quota_bytes > 0),
  updated_at timestamptz NOT NULL,
  updated_by text NOT NULL CHECK (updated_by <> '')
);

CREATE TABLE personal_cloud_quota_audit (
  id text PRIMARY KEY,
  owner_user_id text NOT NULL,
  actor_user_id text NOT NULL CHECK (actor_user_id <> ''),
  request_id text NOT NULL CHECK (request_id <> ''),
  old_quota_bytes bigint,
  new_quota_bytes bigint,
  created_at timestamptz NOT NULL
);

CREATE INDEX personal_cloud_quota_audit_owner_idx
  ON personal_cloud_quota_audit(owner_user_id, created_at);
