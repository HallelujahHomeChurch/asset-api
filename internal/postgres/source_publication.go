package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/auditclient"
)

func (s *Store) GetCollectionSourcePolicy(ctx context.Context, id string) (assets.CollectionSourcePolicy, error) {
	var p assets.CollectionSourcePolicy
	err := s.db.QueryRowContext(ctx, `SELECT id,source_policy_version,acl_revision,binding_generation FROM asset_collections WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&p.CollectionID, &p.Version, &p.ACLRevision, &p.BindingGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return p, assets.ErrNotFound
	}
	return p, err
}

func sourceCollectionMatches(c assets.Collection, caller string, binding, acl int64) error {
	if caller != assets.LineHarnessCaller || c.CreatedByService != caller || c.SourcePolicyVersion != 1 {
		return assets.ErrForbidden
	}
	if binding < 1 || acl < 1 || c.BindingGeneration != binding || c.ACLRevision != acl {
		return assets.ErrConflict
	}
	return nil
}

func (s *Store) StageSourceItem(ctx context.Context, input assets.StageSourceItemInput, now time.Time) (assets.CollectionItemMutation, error) {
	if input.CallerService != assets.LineHarnessCaller {
		return assets.CollectionItemMutation{}, assets.ErrForbidden
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	defer tx.Rollback()
	fingerprint := mutationFingerprint(struct {
		CollectionID, AssetID, RemoteItemID, DisplayName, Revision string
		Binding, ACL                                               int64
	}{input.CollectionID, input.AssetID, input.RemoteItemID, input.DisplayName, input.SourceRevision, input.BindingGeneration, input.ExpectedACLRevision})
	replay, claimed, err := claimMutation(ctx, tx, input.CallerService, "stage_source_item", input.IdempotencyKey, fingerprint, now)
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	if !claimed {
		var v assets.CollectionItemMutation
		err := json.Unmarshal(replay, &v)
		return v, err
	}
	c, err := lockManagedCollection(ctx, tx, input.CollectionID, input.CallerService)
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	if err := sourceCollectionMatches(c, input.CallerService, input.BindingGeneration, input.ExpectedACLRevision); err != nil {
		return assets.CollectionItemMutation{}, err
	}
	if _, err := tx.ExecContext(ctx, `SELECT id FROM assets WHERE id=$1 FOR UPDATE`, input.AssetID); err != nil {
		return assets.CollectionItemMutation{}, err
	}
	a, err := readAsset(ctx, tx, input.AssetID, false)
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	if !a.SourceAvailable(now) || a.UploadStatus != assets.UploadCompleted || a.ScanStatus != assets.ScanNotRequired || a.ProcessingStatus != assets.ProcessingNotRequired || a.ETag == "" {
		return assets.CollectionItemMutation{}, assets.ErrConflict
	}
	item := assets.CollectionItem{ID: newStoreID(), CollectionID: c.ID, AssetID: a.ID, RemoteItemID: input.RemoteItemID, DisplayName: input.DisplayName, SourceRevision: input.SourceRevision, CreatedRevision: c.Revision, UpdatedRevision: c.Revision, MIMEType: a.DetectedMIMEType, SizeBytes: a.SizeBytes, ETag: a.ETag, CreatedAt: now, UpdatedAt: now, PublicationState: "staged", SourceExpiresAt: a.Source.ExpiresAt}
	_, err = tx.ExecContext(ctx, `INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,updated_revision,created_at,updated_at,publication_state,publication_operation) VALUES($1,$2,$3,$4,$5,$6,$7,$7,$8,$8,'staged',$9)`, item.ID, c.ID, a.ID, item.RemoteItemID, item.DisplayName, item.SourceRevision, c.Revision, now, input.IdempotencyKey)
	if err != nil {
		return assets.CollectionItemMutation{}, mapCollectionError(err)
	}
	value := assets.CollectionItemMutation{Collection: c, Item: item}
	err = s.finishAuditedMutation(ctx, tx, input.CallerService, "stage_source_item", input.IdempotencyKey, value, "asset.collection_item.stage", item.ID, auditclient.Metadata{"collectionId": c.ID}, now)
	return value, err
}

func readSourceItem(ctx context.Context, db assetRowReader, collectionID, itemID string) (assets.CollectionItem, error) {
	var i assets.CollectionItem
	err := db.QueryRowContext(ctx, `SELECT i.id,i.collection_id,COALESCE(i.asset_id,''),i.remote_item_id,i.display_name,i.source_revision,i.created_revision,i.updated_revision,COALESCE(i.deleted_revision,0),i.retention_exempt,i.created_at,i.updated_at,COALESCE(i.deleted_at,'0001-01-01'::timestamptz),i.publication_state,COALESCE(a.detected_mime_type,''),COALESCE(a.size_bytes,0),COALESCE(a.etag,''),COALESCE(a.source_expires_at,'0001-01-01'::timestamptz) FROM asset_collection_items i LEFT JOIN assets a ON a.id=i.asset_id WHERE i.collection_id=$1 AND i.id=$2`, collectionID, itemID).Scan(&i.ID, &i.CollectionID, &i.AssetID, &i.RemoteItemID, &i.DisplayName, &i.SourceRevision, &i.CreatedRevision, &i.UpdatedRevision, &i.DeletedRevision, &i.RetentionExempt, &i.CreatedAt, &i.UpdatedAt, &i.DeletedAt, &i.PublicationState, &i.MIMEType, &i.SizeBytes, &i.ETag, &i.SourceExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return i, assets.ErrNotFound
	}
	return i, err
}

func (s *Store) GetSourcePublication(ctx context.Context, collectionID, operation, caller string) (assets.CollectionItemMutation, error) {
	if caller != assets.LineHarnessCaller {
		return assets.CollectionItemMutation{}, assets.ErrForbidden
	}
	c, err := s.ownedSourceCollection(ctx, collectionID, caller)
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	if c.CreatedByService != caller || c.SourcePolicyVersion != 1 {
		return assets.CollectionItemMutation{}, assets.ErrForbidden
	}
	var itemID string
	err = s.db.QueryRowContext(ctx, `SELECT id FROM asset_collection_items WHERE collection_id=$1 AND publication_operation=$2`, collectionID, operation).Scan(&itemID)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.CollectionItemMutation{}, assets.ErrNotFound
	}
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	item, err := readSourceItem(ctx, s.db, collectionID, itemID)
	return assets.CollectionItemMutation{Collection: c, Item: item}, err
}

func (s *Store) ActivateSourceItem(ctx context.Context, input assets.ActivateSourceItemInput, now time.Time) (assets.CollectionItemMutation, error) {
	if input.CallerService != assets.LineHarnessCaller {
		return assets.CollectionItemMutation{}, assets.ErrForbidden
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	defer tx.Rollback()
	fingerprint := mutationFingerprint(struct {
		CollectionID, ItemID string
		Binding, ACL         int64
	}{input.CollectionID, input.ItemID, input.BindingGeneration, input.ExpectedACLRevision})
	replay, claimed, err := claimMutation(ctx, tx, input.CallerService, "activate_source_item", input.IdempotencyKey, fingerprint, now)
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	if !claimed {
		var v assets.CollectionItemMutation
		err := json.Unmarshal(replay, &v)
		return v, err
	}
	c, err := lockManagedCollection(ctx, tx, input.CollectionID, input.CallerService)
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	if err := sourceCollectionMatches(c, input.CallerService, input.BindingGeneration, input.ExpectedACLRevision); err != nil {
		return assets.CollectionItemMutation{}, err
	}
	if _, err := tx.ExecContext(ctx, `SELECT id FROM asset_collection_items WHERE id=$1 AND collection_id=$2 FOR UPDATE`, input.ItemID, c.ID); err != nil {
		return assets.CollectionItemMutation{}, err
	}
	item, err := readSourceItem(ctx, tx, c.ID, input.ItemID)
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	if item.DeletedRevision != 0 || item.AssetID == "" {
		return assets.CollectionItemMutation{}, assets.ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `SELECT id FROM assets WHERE id=$1 FOR UPDATE`, item.AssetID); err != nil {
		return assets.CollectionItemMutation{}, err
	}
	a, err := readAsset(ctx, tx, item.AssetID, false)
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	if !a.SourceAvailable(now) || a.UploadStatus != assets.UploadCompleted || a.ScanStatus != assets.ScanNotRequired || item.ETag != a.ETag {
		return assets.CollectionItemMutation{}, assets.ErrConflict
	}
	if item.PublicationState == "staged" {
		c.Revision++
		c.UpdatedAt = now
		item.PublicationState = "active"
		item.CreatedRevision = c.Revision
		item.UpdatedRevision = c.Revision
		item.UpdatedAt = now
		if _, err := tx.ExecContext(ctx, `UPDATE asset_collection_items SET publication_state='active',created_revision=$2,updated_revision=$2,updated_at=$3 WHERE id=$1`, item.ID, c.Revision, now); err != nil {
			return assets.CollectionItemMutation{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE asset_collections SET revision=$2,updated_at=$3 WHERE id=$1`, c.ID, c.Revision, now); err != nil {
			return assets.CollectionItemMutation{}, err
		}
	}
	value := assets.CollectionItemMutation{Collection: c, Item: item}
	err = s.finishAuditedMutation(ctx, tx, input.CallerService, "activate_source_item", input.IdempotencyKey, value, "asset.collection_item.activate", item.ID, auditclient.Metadata{"collectionId": c.ID}, now)
	return value, err
}

func (s *Store) SourceContentTicket(ctx context.Context, hash string, now time.Time) (assets.ContentTicket, bool, error) {
	var t assets.ContentTicket
	err := s.db.QueryRowContext(ctx, `SELECT t.token_hash,t.collection_id,t.collection_item_id,t.asset_etag,t.user_id,t.access_mode,t.expires_at,t.created_at FROM asset_content_tickets t JOIN asset_collections c ON c.id=t.collection_id WHERE t.token_hash=$1 AND c.source_policy_version<>0 AND t.expires_at>$2`, hash, now).Scan(&t.TokenHash, &t.CollectionID, &t.CollectionItemID, &t.AssetETag, &t.UserID, &t.AccessMode, &t.ExpiresAt, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return t, false, nil
	}
	return t, err == nil, err
}

func (s *Store) GetSourcePublicationItem(ctx context.Context, collectionID, itemID, caller string) (assets.CollectionItemMutation, error) {
	if caller != assets.LineHarnessCaller {
		return assets.CollectionItemMutation{}, assets.ErrForbidden
	}
	c, err := s.ownedSourceCollection(ctx, collectionID, caller)
	if err != nil {
		return assets.CollectionItemMutation{}, err
	}
	if c.CreatedByService != caller || c.SourcePolicyVersion != 1 {
		return assets.CollectionItemMutation{}, assets.ErrForbidden
	}
	item, err := readSourceItem(ctx, s.db, collectionID, itemID)
	return assets.CollectionItemMutation{Collection: c, Item: item}, err
}

// The immutable deadline makes candidate selection safe across the existing
// collection-first soft-delete transaction and its concurrent-reference retry.
func (s *Store) ExpireLineSource(ctx context.Context, now time.Time) (bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM assets WHERE owner_service='hhc-line-harness' AND security_profile='line-source-unscanned-v1' AND source_expires_at<=$1 AND deleted_at IS NULL AND purged_at IS NULL ORDER BY source_expires_at,id LIMIT 1`, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = s.SoftDeleteAsset(ctx, id, assets.LineHarnessCaller, now)
	if errors.Is(err, assets.ErrNotFound) {
		return true, nil
	}
	return err == nil, err
}

func (s *Store) ownedSourceCollection(ctx context.Context, id, caller string) (assets.Collection, error) {
	var c assets.Collection
	err := s.db.QueryRowContext(ctx, `SELECT id,namespace,name,revision,retention_days,created_by_service,created_at,updated_at,acl_revision,source_policy_version,binding_generation,COALESCE(deleted_at,'0001-01-01'::timestamptz) FROM asset_collections WHERE id=$1 AND created_by_service=$2 AND source_policy_version=1`, id, caller).Scan(&c.ID, &c.Namespace, &c.Name, &c.Revision, &c.RetentionDays, &c.CreatedByService, &c.CreatedAt, &c.UpdatedAt, &c.ACLRevision, &c.SourcePolicyVersion, &c.BindingGeneration, &c.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, assets.ErrNotFound
	}
	return c, err
}

func (s *Store) BindSourceCollection(ctx context.Context, input assets.BindSourceCollectionInput, now time.Time) (assets.Collection, error) {
	if input.CallerService != assets.LineHarnessCaller {
		return assets.Collection{}, assets.ErrForbidden
	}
	if input.ExpectedBindingGeneration < 1 || input.BindingGeneration <= input.ExpectedBindingGeneration || input.BindingGeneration > 1<<53-1 {
		return assets.Collection{}, assets.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.Collection{}, err
	}
	defer tx.Rollback()
	fingerprint := mutationFingerprint(struct {
		CollectionID        string
		Previous, Next, ACL int64
	}{input.CollectionID, input.ExpectedBindingGeneration, input.BindingGeneration, input.ExpectedACLRevision})
	replay, claimed, err := claimMutation(ctx, tx, input.CallerService, "bind_source_collection", input.IdempotencyKey, fingerprint, now)
	if err != nil {
		return assets.Collection{}, err
	}
	if !claimed {
		var c assets.Collection
		err := json.Unmarshal(replay, &c)
		return c, err
	}
	c, err := lockManagedCollection(ctx, tx, input.CollectionID, input.CallerService)
	if err != nil {
		return assets.Collection{}, err
	}
	if err := sourceCollectionMatches(c, input.CallerService, input.ExpectedBindingGeneration, input.ExpectedACLRevision); err != nil {
		return assets.Collection{}, err
	}
	c.BindingGeneration = input.BindingGeneration
	c.Revision++
	c.UpdatedAt = now
	if _, err := tx.ExecContext(ctx, `UPDATE asset_collections SET binding_generation=$2,revision=$3,updated_at=$4 WHERE id=$1`, c.ID, c.BindingGeneration, c.Revision, now); err != nil {
		return assets.Collection{}, err
	}
	err = s.finishAuditedMutation(ctx, tx, input.CallerService, "bind_source_collection", input.IdempotencyKey, c, "asset.collection.bind", c.ID, nil, now)
	return c, err
}
