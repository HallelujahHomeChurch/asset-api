ALTER TABLE asset_collections
  ADD COLUMN retention_days smallint NOT NULL DEFAULT 14,
  ADD CONSTRAINT asset_collections_retention_days_check
    CHECK (retention_days BETWEEN 1 AND 365);

ALTER TABLE asset_collection_items
  ADD COLUMN retention_exempt boolean NOT NULL DEFAULT false,
  ADD COLUMN updated_revision bigint,
  ADD COLUMN updated_at timestamptz;

UPDATE asset_collection_items
SET updated_revision = created_revision,
    updated_at = created_at
WHERE updated_revision IS NULL OR updated_at IS NULL;

ALTER TABLE asset_collection_items
  ALTER COLUMN updated_revision SET NOT NULL,
  ALTER COLUMN updated_at SET NOT NULL,
  ADD CONSTRAINT asset_collection_items_updated_revision_check
    CHECK (updated_revision >= created_revision);

ALTER TABLE asset_content_tickets
  ADD COLUMN access_mode text NOT NULL DEFAULT 'reader',
  ADD CONSTRAINT asset_content_tickets_access_mode_check
    CHECK (access_mode IN ('reader', 'manager'));

CREATE INDEX asset_collection_items_managed_list_idx
  ON asset_collection_items (collection_id, created_at DESC, id DESC)
  WHERE deleted_revision IS NULL;

CREATE INDEX asset_collection_items_retention_idx
  ON asset_collection_items (created_at, id)
  WHERE deleted_revision IS NULL AND retention_exempt = false;
