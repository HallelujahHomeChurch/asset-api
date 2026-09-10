CREATE TABLE personal_folder_grants (
  id text PRIMARY KEY CHECK (id <> ''),
  collection_id text NOT NULL,
  folder_item_id text NOT NULL,
  owner_user_id text NOT NULL CHECK (owner_user_id <> ''),
  grantee_user_id text NOT NULL CHECK (grantee_user_id <> ''),
  created_at timestamptz NOT NULL,
  revoked_at timestamptz,
  CHECK (owner_user_id <> grantee_user_id),
  FOREIGN KEY (folder_item_id, collection_id)
    REFERENCES asset_collection_items(id, collection_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX personal_folder_grants_active_idx
  ON personal_folder_grants(collection_id, folder_item_id, grantee_user_id)
  WHERE revoked_at IS NULL;
CREATE INDEX personal_folder_grants_grantee_idx
  ON personal_folder_grants(grantee_user_id, collection_id)
  WHERE revoked_at IS NULL;
