ALTER TABLE asset_collections
 ADD COLUMN acl_revision bigint NOT NULL DEFAULT 1 CHECK (acl_revision > 0),
 ADD COLUMN source_policy_version integer NOT NULL DEFAULT 0 CHECK (source_policy_version IN (0,1)),
 ADD COLUMN binding_generation bigint NOT NULL DEFAULT 0,
 ADD CONSTRAINT source_collection_owner_check CHECK (
  (source_policy_version=0 AND binding_generation=0) OR
  (source_policy_version=1 AND binding_generation>0 AND binding_generation<=9007199254740991 AND created_by_service='hhc-line-harness' AND namespace='line.group.media-sync')
 ) NOT VALID;
ALTER TABLE asset_collection_items
 ADD COLUMN publication_state text NOT NULL DEFAULT 'active' CHECK (publication_state IN ('staged','active')),
 ADD COLUMN publication_operation text;
CREATE UNIQUE INDEX asset_source_publication_operation_idx
 ON asset_collection_items(collection_id,publication_operation) WHERE publication_operation IS NOT NULL;

CREATE FUNCTION asset_acl_revision_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 UPDATE asset_collections SET acl_revision=acl_revision+1 WHERE id=COALESCE(NEW.collection_id,OLD.collection_id);
 RETURN NULL;
END;
$$;
CREATE TRIGGER asset_acl_revision_changed AFTER INSERT OR UPDATE OR DELETE ON asset_collection_acl
 FOR EACH ROW EXECUTE FUNCTION asset_acl_revision_changed();

CREATE FUNCTION asset_source_identity_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.source_policy IS NOT NULL AND
  (NEW.source_policy IS DISTINCT FROM OLD.source_policy OR NEW.source_expires_at IS DISTINCT FROM OLD.source_expires_at OR
   NEW.owner_service IS DISTINCT FROM OLD.owner_service OR NEW.namespace IS DISTINCT FROM OLD.namespace OR
   NEW.security_profile IS DISTINCT FROM OLD.security_profile OR NEW.processing_profile IS DISTINCT FROM OLD.processing_profile)
 THEN RAISE EXCEPTION 'source identity is immutable' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER asset_source_identity_immutable BEFORE UPDATE ON assets
 FOR EACH ROW EXECUTE FUNCTION asset_source_identity_immutable();

CREATE UNIQUE INDEX assets_line_source_original_idx
 ON assets(owner_service,(source_policy->>'ref')) WHERE source_policy IS NOT NULL;
