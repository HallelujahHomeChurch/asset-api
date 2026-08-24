ALTER TABLE asset_content_tickets
  ADD COLUMN role_ids text[] NOT NULL DEFAULT '{}'::text[];

CREATE TABLE asset_collection_acl_audit (
  id text PRIMARY KEY CHECK (id <> ''),
  collection_id text NOT NULL REFERENCES asset_collections(id),
  acl_id text NOT NULL REFERENCES asset_collection_acl(id),
  action text NOT NULL CHECK (action IN ('add', 'revoke')),
  subject_type text NOT NULL CHECK (subject_type IN ('user', 'role')),
  subject_id text NOT NULL CHECK (subject_id <> ''),
  actor_user_id text NOT NULL CHECK (actor_user_id <> ''),
  request_id text NOT NULL CHECK (request_id <> ''),
  created_at timestamptz NOT NULL
);

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'asset') THEN
    REVOKE UPDATE, DELETE, TRUNCATE ON asset_collection_acl_audit FROM asset;
  END IF;
END
$$;
