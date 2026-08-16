CREATE TABLE IF NOT EXISTS asset_collections (
  id text PRIMARY KEY CHECK (id <> ''),
  namespace text NOT NULL CHECK (namespace <> ''),
  name text NOT NULL CHECK (name <> ''),
  revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
  created_by_service text NOT NULL CHECK (created_by_service <> ''),
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  deleted_at timestamptz
);

CREATE TABLE IF NOT EXISTS asset_collection_items (
  id text PRIMARY KEY CHECK (id <> ''),
  collection_id text NOT NULL REFERENCES asset_collections(id),
  asset_id text REFERENCES assets(id) ON DELETE SET NULL,
  remote_item_id text NOT NULL CHECK (remote_item_id <> ''),
  display_name text NOT NULL CHECK (display_name <> ''),
  source_revision text NOT NULL CHECK (source_revision <> ''),
  created_revision bigint NOT NULL CHECK (created_revision > 0),
  deleted_revision bigint,
  created_at timestamptz NOT NULL,
  deleted_at timestamptz,
  CHECK (deleted_revision IS NULL OR deleted_revision >= created_revision),
  UNIQUE (id, collection_id)
);

CREATE UNIQUE INDEX asset_collection_items_active_asset_idx
  ON asset_collection_items(collection_id, asset_id)
  WHERE deleted_revision IS NULL AND asset_id IS NOT NULL;
CREATE UNIQUE INDEX asset_collection_items_active_remote_idx
  ON asset_collection_items(collection_id, remote_item_id)
  WHERE deleted_revision IS NULL;
CREATE INDEX asset_collection_items_created_revision_idx
  ON asset_collection_items(collection_id, created_revision, id);
CREATE INDEX asset_collection_items_deleted_revision_idx
  ON asset_collection_items(collection_id, deleted_revision, id)
  WHERE deleted_revision IS NOT NULL;

CREATE TABLE IF NOT EXISTS asset_collection_acl (
  id text PRIMARY KEY CHECK (id <> ''),
  collection_id text NOT NULL REFERENCES asset_collections(id),
  subject_type text NOT NULL CHECK (subject_type IN ('user', 'role')),
  subject_id text NOT NULL CHECK (subject_id <> ''),
  permission text NOT NULL CHECK (permission = 'read'),
  created_at timestamptz NOT NULL,
  revoked_at timestamptz
);

CREATE UNIQUE INDEX asset_collection_acl_active_subject_idx
  ON asset_collection_acl(collection_id, subject_type, subject_id, permission)
  WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS asset_collection_mutations (
  caller_service text NOT NULL CHECK (caller_service <> ''),
  operation text NOT NULL CHECK (operation <> ''),
  idempotency_key text NOT NULL CHECK (idempotency_key <> ''),
  request_fingerprint text NOT NULL CHECK (request_fingerprint <> ''),
  response_json jsonb,
  created_at timestamptz NOT NULL,
  PRIMARY KEY (caller_service, operation, idempotency_key)
);

CREATE TABLE IF NOT EXISTS asset_content_tickets (
  token_hash text PRIMARY KEY CHECK (token_hash ~ '^[0-9a-f]{64}$'),
  collection_id text NOT NULL REFERENCES asset_collections(id),
  collection_item_id text NOT NULL,
  asset_etag text NOT NULL CHECK (asset_etag <> ''),
  user_id text NOT NULL CHECK (user_id <> ''),
  roles text[] NOT NULL,
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL,
  FOREIGN KEY (collection_item_id, collection_id)
    REFERENCES asset_collection_items(id, collection_id)
);

CREATE INDEX asset_content_tickets_expiry_idx
  ON asset_content_tickets(expires_at);
