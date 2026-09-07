ALTER TABLE asset_collections
  ADD COLUMN owner_user_id text,
  ADD COLUMN permanent_retention boolean NOT NULL DEFAULT false,
  ADD CONSTRAINT personal_collection_owner_check CHECK (
    (namespace = 'presenter.personal' AND owner_user_id IS NOT NULL AND owner_user_id <> '' AND permanent_retention)
    OR (namespace <> 'presenter.personal' AND owner_user_id IS NULL)
  );
CREATE UNIQUE INDEX personal_collection_owner_idx ON asset_collections(owner_user_id)
  WHERE namespace = 'presenter.personal';

ALTER TABLE asset_collection_items
  ADD COLUMN node_kind text NOT NULL DEFAULT 'legacy' CHECK (node_kind IN ('legacy','folder','file')),
  ADD COLUMN parent_item_id text,
  ADD COLUMN deletion_operation_id text,
  ADD CONSTRAINT personal_node_asset_check CHECK (
    node_kind = 'legacy' OR
    (node_kind = 'folder' AND asset_id IS NULL) OR
    (node_kind = 'file' AND (asset_id IS NOT NULL OR deleted_at IS NOT NULL))
  ),
  ADD CONSTRAINT personal_parent_fk FOREIGN KEY (parent_item_id,collection_id)
    REFERENCES asset_collection_items(id,collection_id),
  ADD CONSTRAINT personal_node_retention_check CHECK (node_kind='legacy' OR retention_exempt);
CREATE UNIQUE INDEX personal_node_name_idx
  ON asset_collection_items(collection_id,COALESCE(parent_item_id,''),display_name)
  WHERE node_kind <> 'legacy' AND deleted_at IS NULL;
CREATE INDEX personal_node_parent_idx ON asset_collection_items(collection_id,parent_item_id)
  WHERE node_kind <> 'legacy';

CREATE TABLE personal_sync_receipts (
  owner_user_id text NOT NULL,
  operation_id text NOT NULL,
  request_fingerprint text NOT NULL,
  response_json jsonb NOT NULL,
  created_at timestamptz NOT NULL,
  PRIMARY KEY(owner_user_id,operation_id)
);
CREATE TABLE personal_sync_changes (
  collection_id text NOT NULL REFERENCES asset_collections(id),
  revision bigint NOT NULL CHECK (revision>0),
  item_id text NOT NULL,
  snapshot jsonb NOT NULL,
  created_at timestamptz NOT NULL,
  PRIMARY KEY(collection_id,revision,item_id)
);
