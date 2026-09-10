package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
	"hhc/asset-api/internal/assets"
)

func (s *Store) EnsurePersonalSpace(ctx context.Context, owner string, now time.Time) (assets.PersonalSpace, error) {
	var space assets.PersonalSpace
	if strings.TrimSpace(owner) == "" || len(owner) > 128 {
		return space, assets.ErrUnauthorized
	}
	err := s.db.QueryRowContext(ctx, `INSERT INTO asset_collections(id,namespace,name,revision,created_by_service,created_at,updated_at,owner_user_id,permanent_retention)
 VALUES($1,'presenter.personal','Personal',1,'presenter.personal',$2,$2,$3,true)
 ON CONFLICT(owner_user_id) WHERE namespace='presenter.personal' DO UPDATE SET owner_user_id=EXCLUDED.owner_user_id
	 RETURNING id,revision`, newStoreID(), now, owner).Scan(&space.ID, &space.Revision)
	if err != nil {
		return space, err
	}
	usage, err := s.PersonalUsage(ctx, owner, now)
	space.UsedBytes, space.QuotaBytes = usage.UsedBytes, usage.QuotaBytes
	return space, err
}

type personalUsageQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func personalUsage(ctx context.Context, query personalUsageQuerier, owner string, now time.Time) (assets.PersonalUsage, error) {
	var usage assets.PersonalUsage
	var override sql.NullInt64
	err := query.QueryRowContext(ctx, `WITH owner_collection AS (
  SELECT id FROM asset_collections WHERE owner_user_id=$1 AND namespace='presenter.personal' AND deleted_at IS NULL
), current_refs AS (
  SELECT i.asset_id,
         bool_or(i.deleted_at IS NULL) AS active,
         bool_or(i.deleted_at>$2::timestamptz-interval '30 days') AS trash
  FROM asset_collection_items i JOIN owner_collection c ON c.id=i.collection_id
  WHERE i.node_kind='file' AND i.asset_id IS NOT NULL
  GROUP BY i.asset_id
), protected_refs AS (
  SELECT DISTINCT ch.snapshot->>'assetId' AS asset_id
  FROM personal_sync_changes ch JOIN owner_collection c ON c.id=ch.collection_id
  WHERE COALESCE(ch.snapshot->>'assetId','')<>''
), active AS (
  SELECT COALESCE(SUM(a.size_bytes),0)::bigint AS bytes
  FROM current_refs r JOIN assets a ON a.id=r.asset_id AND a.purged_at IS NULL
  WHERE r.active
), trash AS (
  SELECT COALESCE(SUM(a.size_bytes),0)::bigint AS bytes
  FROM current_refs r JOIN assets a ON a.id=r.asset_id AND a.purged_at IS NULL
  WHERE NOT r.active AND r.trash
), protected AS (
  SELECT COALESCE(SUM(a.size_bytes),0)::bigint AS bytes
  FROM protected_refs r JOIN assets a ON a.id=r.asset_id AND a.purged_at IS NULL
  WHERE NOT EXISTS (SELECT 1 FROM current_refs current WHERE current.asset_id=r.asset_id)
)
SELECT active.bytes,trash.bytes,protected.bytes,
       active.bytes+trash.bytes+protected.bytes,
       COALESCE(q.quota_bytes,$3::bigint),q.quota_bytes
FROM active CROSS JOIN trash CROSS JOIN protected
LEFT JOIN personal_cloud_quotas q ON q.owner_user_id=$1`, owner, now, assets.DefaultPersonalQuotaBytes).Scan(
		&usage.ActiveBytes, &usage.TrashBytes, &usage.ProtectedBytes, &usage.UsedBytes, &usage.QuotaBytes, &override)
	if override.Valid {
		usage.OverrideBytes = &override.Int64
	}
	return usage, err
}

func (s *Store) PersonalUsage(ctx context.Context, owner string, now time.Time) (assets.PersonalUsage, error) {
	if strings.TrimSpace(owner) == "" || len(owner) > 128 {
		return assets.PersonalUsage{}, assets.ErrUnauthorized
	}
	return personalUsage(ctx, s.db, owner, now)
}

func (s *Store) SetPersonalQuota(ctx context.Context, actor, owner string, quota *int64, requestID string, now time.Time) (assets.PersonalUsage, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return assets.PersonalUsage{}, err
	}
	defer tx.Rollback()
	var collectionID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM asset_collections WHERE owner_user_id=$1 AND namespace='presenter.personal' AND deleted_at IS NULL FOR UPDATE`, owner).Scan(&collectionID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return assets.PersonalUsage{}, err
	}
	var old sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT quota_bytes FROM personal_cloud_quotas WHERE owner_user_id=$1 FOR UPDATE`, owner).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return assets.PersonalUsage{}, err
	}
	if quota == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM personal_cloud_quotas WHERE owner_user_id=$1`, owner)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO personal_cloud_quotas(owner_user_id,quota_bytes,updated_at,updated_by) VALUES($1,$2,$3,$4)
 ON CONFLICT(owner_user_id) DO UPDATE SET quota_bytes=EXCLUDED.quota_bytes,updated_at=EXCLUDED.updated_at,updated_by=EXCLUDED.updated_by`, owner, *quota, now, actor)
	}
	if err != nil {
		return assets.PersonalUsage{}, err
	}
	var oldValue, newValue any
	if old.Valid {
		oldValue = old.Int64
	}
	if quota != nil {
		newValue = *quota
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO personal_cloud_quota_audit(id,owner_user_id,actor_user_id,request_id,old_quota_bytes,new_quota_bytes,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, newStoreID(), owner, actor, requestID, oldValue, newValue, now); err != nil {
		return assets.PersonalUsage{}, err
	}
	usage, err := personalUsage(ctx, tx, owner, now)
	if err != nil {
		return assets.PersonalUsage{}, err
	}
	if err = tx.Commit(); err != nil {
		return assets.PersonalUsage{}, err
	}
	return usage, nil
}

const personalNodeColumns = `id,collection_id,COALESCE(parent_item_id,''),node_kind,display_name,COALESCE(asset_id,''),updated_revision,deleted_at,COALESCE(deletion_operation_id,'')`

func scanPersonalNode(row interface{ Scan(...any) error }) (assets.PersonalNode, error) {
	var n assets.PersonalNode
	err := row.Scan(&n.ID, &n.CollectionID, &n.ParentID, &n.Kind, &n.Name, &n.AssetID, &n.Revision, &n.DeletedAt, &n.DeletionOperationID)
	if errors.Is(err, sql.ErrNoRows) {
		err = assets.ErrNotFound
	}
	return n, err
}
func (s *Store) GetPersonalNode(ctx context.Context, owner, id string) (assets.PersonalNode, error) {
	return scanPersonalNode(s.db.QueryRowContext(ctx, `SELECT `+personalNodeColumns+` FROM asset_collection_items WHERE id=$1 AND node_kind<>'legacy' AND collection_id IN (SELECT id FROM asset_collections WHERE owner_user_id=$2 AND namespace='presenter.personal' AND deleted_at IS NULL)`, id, owner))
}
func personalNode(ctx context.Context, tx *sql.Tx, collection, id string) (assets.PersonalNode, error) {
	return scanPersonalNode(tx.QueryRowContext(ctx, `SELECT `+personalNodeColumns+` FROM asset_collection_items WHERE collection_id=$1 AND id=$2 AND node_kind<>'legacy'`, collection, id))
}
func personalParent(ctx context.Context, tx *sql.Tx, collection, parent string) error {
	if parent == "" {
		return nil
	}
	n, err := personalNode(ctx, tx, collection, parent)
	if err != nil {
		return err
	}
	if n.Kind != "folder" || n.DeletedAt != nil {
		return assets.ErrConflict
	}
	return nil
}

func (s *Store) ApplyPersonalMutation(ctx context.Context, owner string, m assets.PersonalMutation, now time.Time) (assets.PersonalMutationResult, error) {
	var result assets.PersonalMutationResult
	if owner == "" {
		return result, assets.ErrUnauthorized
	}
	if err := m.Validate(); err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var space assets.PersonalSpace
	// ponytail: per-collection lock; revisit only if personal sync contention is measured.
	err = tx.QueryRowContext(ctx, `SELECT id,revision FROM asset_collections WHERE owner_user_id=$1 AND namespace='presenter.personal' AND deleted_at IS NULL FOR UPDATE`, owner).Scan(&space.ID, &space.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return result, assets.ErrNotFound
	}
	if err != nil {
		return result, err
	}
	fingerprint := mutationFingerprint(m)
	var savedHash string
	var saved []byte
	err = tx.QueryRowContext(ctx, `SELECT request_fingerprint,response_json FROM personal_sync_receipts WHERE owner_user_id=$1 AND operation_id=$2`, owner, m.OperationID).Scan(&savedHash, &saved)
	if err == nil {
		if savedHash != fingerprint {
			return result, assets.ErrConflict
		}
		err = json.Unmarshal(saved, &result)
		return result, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	creating := m.Type == "create-folder" || m.Type == "create-file"
	var node assets.PersonalNode
	if creating {
		node = assets.PersonalNode{ID: m.ItemID, CollectionID: space.ID, ParentID: m.ParentID, Name: norm.NFC.String(m.Name), Kind: strings.TrimPrefix(m.Type, "create-")}
	} else {
		node, err = personalNode(ctx, tx, space.ID, m.ItemID)
		if err != nil {
			return result, err
		}
		if node.Revision != m.ExpectedRevision {
			return result, assets.ErrConflict
		}
		if (node.DeletedAt != nil) != (m.Type == "restore") {
			return result, assets.ErrConflict
		}
	}
	previousAssetID := node.AssetID
	switch m.Type {
	case "create-file", "replace-content":
		if node.Kind != "file" {
			return result, assets.ErrConflict
		}
		var upload, scan, processing string
		err = tx.QueryRowContext(ctx, `SELECT a.id,a.upload_status,a.scan_status,a.processing_status FROM assets a JOIN upload_sessions u ON u.asset_id=a.id
   WHERE u.id=$1 AND a.namespace='presenter.personal' AND a.owner_type='user' AND a.owner_id=$2 AND a.owner_service='presenter.personal'
   AND a.deleted_at IS NULL AND a.purged_at IS NULL AND (a.purge_claimed_until IS NULL OR a.purge_claimed_until<$3) FOR UPDATE OF a`, m.UploadID, owner, now).Scan(&node.AssetID, &upload, &scan, &processing)
		if errors.Is(err, sql.ErrNoRows) {
			return result, assets.ErrNotFound
		}
		if err != nil {
			return result, err
		}
		if upload == "failed" || scan == "infected" || scan == "failed" || processing == "failed" {
			return result, assets.ErrInvalidUpload
		}
		if upload != "completed" || scan != "clean" || (processing != "ready" && processing != "not_required") {
			return result, assets.ErrPersonalAssetNotReady
		}
		usage, e := personalUsage(ctx, tx, owner, now)
		if e != nil {
			return result, e
		}
		var size int64
		if e = tx.QueryRowContext(ctx, `SELECT size_bytes FROM assets WHERE id=$1`, node.AssetID).Scan(&size); e != nil {
			return result, e
		}
		var counted bool
		e = tx.QueryRowContext(ctx, `SELECT EXISTS(
  SELECT 1 FROM asset_collection_items i WHERE i.collection_id=$1 AND i.asset_id=$2 AND i.node_kind='file'
    AND (i.deleted_at IS NULL OR i.deleted_at>$3::timestamptz-interval '30 days')
  UNION ALL
  SELECT 1 FROM personal_sync_changes ch WHERE ch.collection_id=$1 AND ch.snapshot->>'assetId'=$2
    AND NOT EXISTS (SELECT 1 FROM asset_collection_items current WHERE current.collection_id=$1 AND current.asset_id=$2 AND current.node_kind='file')
)`, space.ID, node.AssetID, now).Scan(&counted)
		if e != nil {
			return result, e
		}
		required := size
		if counted {
			required = 0
		}
		if required > usage.QuotaBytes-usage.UsedBytes {
			return result, &assets.PersonalQuotaExceeded{UsedBytes: usage.UsedBytes, QuotaBytes: usage.QuotaBytes, RequiredBytes: required}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE assets SET personal_download_until=GREATEST(personal_download_until,$2::timestamptz+interval '10 minutes') WHERE id=$1 OR id=NULLIF($3,'')`, node.AssetID, now, previousAssetID); err != nil {
			return result, err
		}
	case "rename":
		node.Name = norm.NFC.String(m.Name)
	case "restore":
		if m.Name != "" {
			node.Name = norm.NFC.String(m.Name)
		}
	case "move":
		node.ParentID = m.ParentID
		var cycle bool
		err = tx.QueryRowContext(ctx, `WITH RECURSIVE ancestors AS (SELECT id,parent_item_id FROM asset_collection_items WHERE collection_id=$1 AND id=$2 UNION SELECT p.id,p.parent_item_id FROM asset_collection_items p JOIN ancestors a ON p.id=a.parent_item_id WHERE p.collection_id=$1) SELECT EXISTS(SELECT 1 FROM ancestors WHERE id=$3)`, space.ID, m.ParentID, node.ID).Scan(&cycle)
		if err != nil {
			return result, err
		}
		if cycle {
			return result, assets.ErrConflict
		}
	}
	if m.Type == "restore" && node.ParentID != "" {
		parent, e := personalNode(ctx, tx, space.ID, node.ParentID)
		if e != nil && !errors.Is(e, assets.ErrNotFound) {
			return result, e
		}
		if e != nil || parent.DeletedAt != nil {
			node.ParentID = ""
		}
	}
	if err = personalParent(ctx, tx, space.ID, node.ParentID); err != nil {
		return result, err
	}
	nodes := []assets.PersonalNode{node}
	if m.Type == "delete" || m.Type == "restore" {
		if node.Kind == "folder" {
			if m.ExpectedCollectionRevision != space.Revision {
				return result, assets.ErrConflict
			}
			rows, e := tx.QueryContext(ctx, `WITH RECURSIVE subtree AS (SELECT id FROM asset_collection_items WHERE collection_id=$1 AND parent_item_id=$2
    UNION ALL SELECT i.id FROM asset_collection_items i JOIN subtree s ON i.parent_item_id=s.id WHERE i.collection_id=$1)
    SELECT `+personalNodeColumns+` FROM asset_collection_items WHERE id IN (SELECT id FROM subtree)
    AND (($3='delete' AND deleted_at IS NULL) OR ($3='restore' AND deletion_operation_id=$4)) ORDER BY id`, space.ID, node.ID, m.Type, node.DeletionOperationID)
			if e != nil {
				return result, e
			}
			for rows.Next() {
				child, e := scanPersonalNode(rows)
				if e != nil {
					rows.Close()
					return result, e
				}
				nodes = append(nodes, child)
			}
			if e = finishRows(rows); e != nil {
				return result, e
			}
		}
		for i := range nodes {
			if m.Type == "delete" {
				nodes[i].DeletedAt = &now
				nodes[i].DeletionOperationID = m.OperationID
			} else {
				if nodes[i].DeletedAt == nil || !nodes[i].DeletedAt.After(now.Add(-30*24*time.Hour)) {
					return result, assets.ErrNotFound
				}
				if nodes[i].Kind == "file" {
					updated, e := tx.ExecContext(ctx, `UPDATE assets SET personal_download_until=GREATEST(personal_download_until,$2::timestamptz+interval '10 minutes') WHERE id=$1 AND deleted_at IS NULL AND purged_at IS NULL AND upload_status='completed' AND scan_status='clean' AND processing_status IN('ready','not_required') AND (purge_claimed_until IS NULL OR purge_claimed_until<$2)`, nodes[i].AssetID, now)
					if e != nil {
						return result, e
					}
					count, e := updated.RowsAffected()
					if e != nil {
						return result, e
					}
					if count != 1 {
						return result, assets.ErrNotFound
					}
				}
				nodes[i].DeletedAt = nil
				nodes[i].DeletionOperationID = ""
			}
		}
	}
	revision := space.Revision + 1
	if _, err = tx.ExecContext(ctx, `UPDATE asset_collections SET revision=$2,updated_at=$3 WHERE id=$1`, space.ID, revision, now); err != nil {
		return result, err
	}
	for i := range nodes {
		n := &nodes[i]
		n.Revision = revision
		if creating {
			_, err = tx.ExecContext(ctx, `INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,updated_revision,created_at,updated_at,node_kind,parent_item_id,retention_exempt)
    VALUES($1,$2,NULLIF($3,''),$1,$4,$5,$6,$6,$7,$7,$8,NULLIF($9,''),true)`, n.ID, space.ID, n.AssetID, n.Name, strconv.FormatInt(revision, 10), revision, now, n.Kind, n.ParentID)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE asset_collection_items SET asset_id=NULLIF($3,''),parent_item_id=NULLIF($4,''),display_name=$5,source_revision=$6,updated_revision=$7::bigint,updated_at=$8,deleted_at=$9,deleted_revision=CASE WHEN $9::timestamptz IS NULL THEN NULL ELSE $7::bigint END,deletion_operation_id=NULLIF($10,'') WHERE id=$1 AND collection_id=$2`, n.ID, space.ID, n.AssetID, n.ParentID, n.Name, strconv.FormatInt(revision, 10), revision, now, n.DeletedAt, n.DeletionOperationID)
		}
		if err != nil {
			return result, mapCollectionError(err)
		}
		snapshot, e := json.Marshal(n)
		if e != nil {
			return result, e
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO personal_sync_changes(collection_id,revision,item_id,snapshot,created_at) VALUES($1,$2,$3,$4,$5)`, space.ID, revision, n.ID, snapshot, now); err != nil {
			return result, err
		}
	}
	result = assets.PersonalMutationResult{ItemID: node.ID, NodeRevision: revision, CollectionRevision: revision}
	encoded, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO personal_sync_receipts(owner_user_id,operation_id,request_fingerprint,response_json,created_at) VALUES($1,$2,$3,$4,$5)`, owner, m.OperationID, fingerprint, encoded, now); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return assets.PersonalMutationResult{}, err
	}
	return result, nil
}

func (s *Store) PurgePersonalTrash(ctx context.Context, owner string, input assets.PersonalTrashPurgeInput, now time.Time) (assets.PersonalTrashPurgeResult, error) {
	var result assets.PersonalTrashPurgeResult
	if err := input.Validate(); err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var space assets.PersonalSpace
	err = tx.QueryRowContext(ctx, `SELECT id,revision FROM asset_collections WHERE owner_user_id=$1 AND namespace='presenter.personal' AND deleted_at IS NULL FOR UPDATE`, owner).Scan(&space.ID, &space.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return result, assets.ErrNotFound
	}
	if err != nil {
		return result, err
	}
	receiptID := "purge:" + input.OperationID
	fingerprint := mutationFingerprint(input)
	var savedHash string
	var saved []byte
	err = tx.QueryRowContext(ctx, `SELECT request_fingerprint,response_json FROM personal_sync_receipts WHERE owner_user_id=$1 AND operation_id=$2`, owner, receiptID).Scan(&savedHash, &saved)
	if err == nil {
		if savedHash != fingerprint {
			return result, assets.ErrConflict
		}
		err = json.Unmarshal(saved, &result)
		return result, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if !input.All {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM asset_collection_items WHERE collection_id=$1 AND id=ANY($2::text[]) AND node_kind<>'legacy' AND deleted_at IS NOT NULL`, space.ID, input.ItemIDs).Scan(&count); err != nil {
			return result, err
		}
		if count != len(input.ItemIDs) {
			return result, assets.ErrNotFound
		}
	}
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE selected AS (
  SELECT id FROM asset_collection_items WHERE collection_id=$1 AND deleted_at IS NOT NULL
    AND ($2::boolean OR id=ANY($3::text[]))
  UNION
  SELECT child.id FROM asset_collection_items child JOIN selected parent ON child.parent_item_id=parent.id
  WHERE child.collection_id=$1 AND child.deleted_at IS NOT NULL
)
SELECT `+personalNodeColumns+` FROM asset_collection_items WHERE collection_id=$1 AND id IN(SELECT id FROM selected) ORDER BY id`, space.ID, input.All, input.ItemIDs)
	if err != nil {
		return result, err
	}
	var nodes []assets.PersonalNode
	for rows.Next() {
		node, scanErr := scanPersonalNode(rows)
		if scanErr != nil {
			rows.Close()
			return result, scanErr
		}
		nodes = append(nodes, node)
		result.PurgedItemIDs = append(result.PurgedItemIDs, node.ID)
	}
	if err = finishRows(rows); err != nil {
		return result, err
	}
	if len(nodes) > 0 {
		revision := space.Revision + 1
		if _, err = tx.ExecContext(ctx, `UPDATE asset_collection_items SET parent_item_id=NULL WHERE collection_id=$1 AND deleted_at IS NULL AND parent_item_id=ANY($2::text[])`, space.ID, result.PurgedItemIDs); err != nil {
			return result, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM personal_sync_changes WHERE collection_id=$1 AND item_id=ANY($2::text[])`, space.ID, result.PurgedItemIDs); err != nil {
			return result, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM asset_collection_items WHERE collection_id=$1 AND id=ANY($2::text[])`, space.ID, result.PurgedItemIDs); err != nil {
			return result, err
		}
		for _, node := range nodes {
			node.AssetID, node.ParentID, node.DeletionOperationID = "", "", ""
			node.Revision, node.Purged = revision, true
			snapshot, encodeErr := json.Marshal(node)
			if encodeErr != nil {
				return result, encodeErr
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO personal_sync_changes(collection_id,revision,item_id,snapshot,created_at) VALUES($1,$2,$3,$4,$5)`, space.ID, revision, node.ID, snapshot, now); err != nil {
				return result, err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE asset_collections SET revision=$2,updated_at=$3 WHERE id=$1`, space.ID, revision, now); err != nil {
			return result, err
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO personal_sync_receipts(owner_user_id,operation_id,request_fingerprint,response_json,created_at) VALUES($1,$2,$3,$4,$5)`, owner, receiptID, fingerprint, encoded, now); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return assets.PersonalTrashPurgeResult{}, err
	}
	return result, nil
}

// PersonalChanges reads immutable snapshots, so concurrent writes cannot alter an in-flight page set.
func (s *Store) PersonalChanges(ctx context.Context, owner, cursor string, limit int) (assets.PersonalChangePage, error) {
	page := assets.PersonalChangePage{Items: []assets.PersonalNode{}}
	err := s.db.QueryRowContext(ctx, `SELECT id,revision FROM asset_collections WHERE owner_user_id=$1 AND namespace='presenter.personal' AND deleted_at IS NULL`, owner).Scan(&page.Collection.ID, &page.Collection.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return page, assets.ErrNotFound
	}
	if err != nil {
		return page, err
	}
	c, valid := decodeChangeCursor(cursor)
	if !valid || c.CollectionID != page.Collection.ID || (c.Mode != "personal-reset" && c.Mode != "personal-delta") || c.FromRevision < 0 || c.HighWater < 0 || c.HighWater > page.Collection.Revision || c.FromRevision > page.Collection.Revision || c.AfterRevision < 0 || c.AfterRevision > c.HighWater || (c.HighWater > 0 && c.FromRevision > c.HighWater) {
		c = changeCursor{Mode: "personal-reset", CollectionID: page.Collection.ID, HighWater: page.Collection.Revision}
	}
	if c.HighWater == 0 {
		c.HighWater = page.Collection.Revision
	}
	page.Reset = c.Mode == "personal-reset"
	limit = boundedCollectionLimit(limit)
	var rows *sql.Rows
	if page.Reset {
		rows, err = s.db.QueryContext(ctx, `SELECT snapshot FROM (SELECT DISTINCT ON(item_id) item_id,snapshot FROM personal_sync_changes WHERE collection_id=$1 AND revision<=$2 AND item_id>$3 ORDER BY item_id,revision DESC) latest ORDER BY item_id LIMIT $4`, c.CollectionID, c.HighWater, c.AfterID, limit+1)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT snapshot FROM personal_sync_changes WHERE collection_id=$1 AND revision>$2 AND revision<=$3 AND (revision,item_id)>($4,$5) ORDER BY revision,item_id LIMIT $6`, c.CollectionID, c.FromRevision, c.HighWater, c.AfterRevision, c.AfterID, limit+1)
	}
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var raw []byte
		var node assets.PersonalNode
		if err = rows.Scan(&raw); err == nil {
			err = json.Unmarshal(raw, &node)
		}
		if err != nil {
			rows.Close()
			return page, err
		}
		page.Items = append(page.Items, node)
	}
	if err = finishRows(rows); err != nil {
		return page, err
	}
	page.HasMore = len(page.Items) > limit
	if page.HasMore {
		page.Items = page.Items[:limit]
		last := page.Items[len(page.Items)-1]
		c.AfterID = last.ID
		if !page.Reset {
			c.AfterRevision = last.Revision
		}
	} else {
		c = changeCursor{Mode: "personal-delta", CollectionID: c.CollectionID, FromRevision: c.HighWater}
	}
	page.Cursor = encodeChangeCursor(c)
	return page, nil
}

func (s *Store) PersonalUploadAssetID(ctx context.Context, owner, uploadID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT a.id FROM assets a JOIN upload_sessions u ON u.asset_id=a.id WHERE u.id=$1 AND a.namespace='presenter.personal' AND a.owner_service='presenter.personal' AND a.owner_type='user' AND a.owner_id=$2 AND a.deleted_at IS NULL AND a.purged_at IS NULL`, uploadID, owner).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", assets.ErrNotFound
	}
	return id, err
}

// A lease and the purge claim compete for the same asset row before any Blob I/O.
func (s *Store) PersonalContentAssetID(ctx context.Context, owner, item string, revision int64, now time.Time) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `WITH owned AS (
  SELECT i.id,i.collection_id,i.asset_id FROM asset_collection_items i JOIN asset_collections c ON c.id=i.collection_id
  WHERE i.id=$1 AND c.owner_user_id=$2 AND c.namespace='presenter.personal' AND c.deleted_at IS NULL AND i.node_kind='file'
  AND (i.deleted_at IS NULL OR i.deleted_at>$4::timestamptz-interval '30 days')
 ), selected AS (
  SELECT asset_id FROM owned WHERE $3::bigint=0
  UNION ALL
  SELECT ch.snapshot->>'assetId' FROM personal_sync_changes ch JOIN owned o ON o.id=ch.item_id AND o.collection_id=ch.collection_id
  WHERE ch.revision=$3::bigint AND NOT ch.snapshot ? 'deletedAt'
 ) UPDATE assets a SET personal_download_until=GREATEST(a.personal_download_until,$4::timestamptz+interval '10 minutes')
 WHERE a.id IN(SELECT asset_id FROM selected) AND a.owner_id=$2 AND a.namespace='presenter.personal'
 AND a.upload_status='completed' AND a.scan_status='clean' AND a.processing_status IN('ready','not_required')
 AND a.deleted_at IS NULL AND a.purged_at IS NULL AND (a.purge_claimed_until IS NULL OR a.purge_claimed_until<$4)
 RETURNING a.id`, item, owner, revision, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", assets.ErrNotFound
	}
	return id, err
}
