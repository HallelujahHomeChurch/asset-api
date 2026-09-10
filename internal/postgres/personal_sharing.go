package postgres

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"hhc/asset-api/internal/assets"
)

type sharedCursor struct {
	Recipient string `json:"recipient"`
	GrantID   string `json:"grantId"`
	Revision  int64  `json:"revision"`
	AfterID   string `json:"afterId"`
}

func encodeSharedCursor(cursor sharedCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeSharedCursor(value string) (sharedCursor, bool) {
	if value == "" {
		return sharedCursor{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return sharedCursor{}, false
	}
	var cursor sharedCursor
	err = json.Unmarshal(raw, &cursor)
	return cursor, err == nil
}

func scanPersonalFolderGrant(row interface{ Scan(...any) error }) (assets.PersonalFolderGrant, error) {
	var grant assets.PersonalFolderGrant
	err := row.Scan(&grant.ID, &grant.FolderItemID, &grant.OwnerUserID, &grant.GranteeUserID, &grant.CreatedAt, &grant.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = assets.ErrNotFound
	}
	return grant, err
}

func (s *Store) CreatePersonalFolderGrant(ctx context.Context, owner, item, grantee string, now time.Time) (assets.PersonalFolderGrant, error) {
	if owner == grantee {
		return assets.PersonalFolderGrant{}, assets.ErrConflict
	}
	return scanPersonalFolderGrant(s.db.QueryRowContext(ctx, `WITH candidate AS (
  SELECT i.collection_id FROM asset_collection_items i
  JOIN asset_collections c ON c.id=i.collection_id
  WHERE i.id=$2 AND c.owner_user_id=$1 AND c.namespace='presenter.personal'
    AND c.deleted_at IS NULL AND i.node_kind='folder' AND i.deleted_at IS NULL
), inserted AS (
  INSERT INTO personal_folder_grants(id,collection_id,folder_item_id,owner_user_id,grantee_user_id,created_at)
  SELECT $5,collection_id,$2,$1,$3,$4 FROM candidate
  ON CONFLICT (collection_id,folder_item_id,grantee_user_id) WHERE revoked_at IS NULL
  DO UPDATE SET folder_item_id=EXCLUDED.folder_item_id
  RETURNING id,folder_item_id,owner_user_id,grantee_user_id,created_at,revoked_at
)
SELECT * FROM inserted`, owner, item, grantee, now, newStoreID()))
}

func (s *Store) ListPersonalFolderGrants(ctx context.Context, owner, item string) ([]assets.PersonalFolderGrant, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM asset_collection_items i JOIN asset_collections c ON c.id=i.collection_id WHERE i.id=$2 AND c.owner_user_id=$1 AND c.namespace='presenter.personal' AND c.deleted_at IS NULL AND i.node_kind='folder' AND i.deleted_at IS NULL)`, owner, item).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, assets.ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT g.id,g.folder_item_id,g.owner_user_id,g.grantee_user_id,g.created_at,g.revoked_at
FROM personal_folder_grants g JOIN asset_collection_items i ON i.id=g.folder_item_id AND i.collection_id=g.collection_id
JOIN asset_collections c ON c.id=g.collection_id
WHERE g.owner_user_id=$1 AND g.folder_item_id=$2 AND g.revoked_at IS NULL
  AND c.owner_user_id=$1 AND c.namespace='presenter.personal' AND c.deleted_at IS NULL AND i.deleted_at IS NULL
ORDER BY g.created_at,g.id`, owner, item)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := []assets.PersonalFolderGrant{}
	for rows.Next() {
		grant, scanErr := scanPersonalFolderGrant(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

func (s *Store) RevokePersonalFolderGrant(ctx context.Context, owner, item, grant string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE personal_folder_grants SET revoked_at=COALESCE(revoked_at,$4)
WHERE id=$3 AND owner_user_id=$1 AND folder_item_id=$2`, owner, item, grant, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return assets.ErrNotFound
	}
	return nil
}

func (s *Store) LeaveSharedFolder(ctx context.Context, recipient, grant string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE personal_folder_grants SET revoked_at=COALESCE(revoked_at,$3)
WHERE id=$2 AND grantee_user_id=$1`, recipient, grant, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return assets.ErrNotFound
	}
	return nil
}

func (s *Store) ListSharedFolderRoots(ctx context.Context, recipient string) ([]assets.SharedFolderRoot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT g.id,g.owner_user_id,root.id,root.collection_id,COALESCE(root.parent_item_id,''),root.node_kind,root.display_name,COALESCE(root.asset_id,''),root.updated_revision,root.deleted_at,COALESCE(root.deletion_operation_id,''),c.revision
FROM personal_folder_grants g
JOIN asset_collections c ON c.id=g.collection_id AND c.namespace='presenter.personal' AND c.deleted_at IS NULL
JOIN asset_collection_items root ON root.id=g.folder_item_id AND root.collection_id=g.collection_id
WHERE g.grantee_user_id=$1 AND g.revoked_at IS NULL AND root.node_kind='folder' AND root.deleted_at IS NULL
AND NOT EXISTS (
  WITH RECURSIVE ancestors AS (
    SELECT parent.id,parent.parent_item_id,parent.deleted_at FROM asset_collection_items parent
    WHERE parent.collection_id=g.collection_id AND parent.id=root.parent_item_id
    UNION ALL
    SELECT parent.id,parent.parent_item_id,parent.deleted_at FROM asset_collection_items parent
    JOIN ancestors child ON child.parent_item_id=parent.id WHERE parent.collection_id=g.collection_id
  )
  SELECT 1 FROM ancestors a JOIN personal_folder_grants parent_grant
    ON parent_grant.collection_id=g.collection_id AND parent_grant.folder_item_id=a.id
   AND parent_grant.grantee_user_id=$1 AND parent_grant.revoked_at IS NULL
)
AND NOT EXISTS (
  WITH RECURSIVE ancestors AS (
    SELECT parent.id,parent.parent_item_id,parent.deleted_at FROM asset_collection_items parent
    WHERE parent.collection_id=g.collection_id AND parent.id=root.parent_item_id
    UNION ALL
    SELECT parent.id,parent.parent_item_id,parent.deleted_at FROM asset_collection_items parent
    JOIN ancestors child ON child.parent_item_id=parent.id WHERE parent.collection_id=g.collection_id
  ) SELECT 1 FROM ancestors WHERE deleted_at IS NOT NULL
)
ORDER BY root.display_name,g.id`, recipient)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	roots := []assets.SharedFolderRoot{}
	for rows.Next() {
		var root assets.SharedFolderRoot
		var node assets.PersonalNode
		if err = rows.Scan(&root.GrantID, &root.OwnerUserID, &node.ID, &node.CollectionID, &node.ParentID, &node.Kind, &node.Name, &node.AssetID, &node.Revision, &node.DeletedAt, &node.DeletionOperationID, &root.CollectionRevision); err != nil {
			return nil, err
		}
		root.Root = node
		roots = append(roots, root)
	}
	return roots, rows.Err()
}

func (s *Store) SharedFolderSnapshot(ctx context.Context, recipient, grant, cursorValue string, limit int) (assets.SharedFolderSnapshot, error) {
	page := assets.SharedFolderSnapshot{GrantID: grant, Items: []assets.PersonalNode{}, Reset: true}
	var collectionID, rootID string
	err := s.db.QueryRowContext(ctx, `SELECT g.collection_id,g.folder_item_id,c.revision
FROM personal_folder_grants g JOIN asset_collections c ON c.id=g.collection_id
JOIN asset_collection_items root ON root.id=g.folder_item_id AND root.collection_id=g.collection_id
WHERE g.id=$1 AND g.grantee_user_id=$2 AND g.revoked_at IS NULL AND c.deleted_at IS NULL AND root.deleted_at IS NULL
AND NOT EXISTS (
  WITH RECURSIVE ancestors AS (
    SELECT parent.id,parent.parent_item_id,parent.deleted_at FROM asset_collection_items parent
    WHERE parent.collection_id=g.collection_id AND parent.id=root.parent_item_id
    UNION ALL
    SELECT parent.id,parent.parent_item_id,parent.deleted_at FROM asset_collection_items parent
    JOIN ancestors child ON child.parent_item_id=parent.id WHERE parent.collection_id=g.collection_id
  ) SELECT 1 FROM ancestors WHERE deleted_at IS NOT NULL
)`, grant, recipient).Scan(&collectionID, &rootID, &page.CollectionRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return page, assets.ErrNotFound
	}
	if err != nil {
		return page, err
	}
	cursor, valid := decodeSharedCursor(cursorValue)
	if cursorValue != "" && (!valid || cursor.Recipient != recipient || cursor.GrantID != grant || cursor.Revision <= 0 || cursor.Revision > page.CollectionRevision) {
		return page, assets.ErrInvalidInput
	}
	if !valid {
		cursor = sharedCursor{Recipient: recipient, GrantID: grant, Revision: page.CollectionRevision}
	}
	page.CollectionRevision = cursor.Revision
	limit = boundedCollectionLimit(limit)
	rows, err := s.db.QueryContext(ctx, `WITH RECURSIVE latest AS (
  SELECT DISTINCT ON(item_id) item_id,snapshot FROM personal_sync_changes
  WHERE collection_id=$1 AND revision<=$2 ORDER BY item_id,revision DESC
), subtree AS (
  SELECT item_id,snapshot FROM latest WHERE item_id=$3 AND NOT snapshot ? 'deletedAt' AND COALESCE((snapshot->>'purged')::boolean,false)=false
  UNION ALL
  SELECT child.item_id,child.snapshot FROM latest child JOIN subtree parent ON child.snapshot->>'parentId'=parent.item_id
  WHERE NOT child.snapshot ? 'deletedAt' AND COALESCE((child.snapshot->>'purged')::boolean,false)=false
)
SELECT subtree.snapshot,COALESCE(NULLIF(a.detected_mime_type,''),a.expected_mime_type,''),COALESCE(a.size_bytes,0),COALESCE(a.etag,'')
FROM subtree LEFT JOIN assets a ON a.id=subtree.snapshot->>'assetId'
WHERE subtree.item_id>$4 ORDER BY subtree.item_id LIMIT $5`, collectionID, cursor.Revision, rootID, cursor.AfterID, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var node assets.PersonalNode
		if err = rows.Scan(&raw, &node.MimeType, &node.SizeBytes, &node.ETag); err == nil {
			err = json.Unmarshal(raw, &node)
		}
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, node)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	page.HasMore = len(page.Items) > limit
	if page.HasMore {
		page.Items = page.Items[:limit]
		cursor.AfterID = page.Items[len(page.Items)-1].ID
		page.Cursor = encodeSharedCursor(cursor)
	}
	return page, nil
}

func (s *Store) SharedFolderContentAssetID(ctx context.Context, recipient, grant, item string, now time.Time) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `WITH RECURSIVE readable AS (
  SELECT root.id,root.collection_id,root.parent_item_id,root.asset_id,root.node_kind FROM personal_folder_grants g
  JOIN asset_collection_items root ON root.id=g.folder_item_id AND root.collection_id=g.collection_id
  JOIN asset_collections c ON c.id=g.collection_id AND c.namespace='presenter.personal' AND c.deleted_at IS NULL
  WHERE g.id=$1 AND g.grantee_user_id=$2 AND g.revoked_at IS NULL AND root.deleted_at IS NULL
  AND NOT EXISTS (
    WITH RECURSIVE ancestors AS (
      SELECT parent.id,parent.parent_item_id,parent.deleted_at FROM asset_collection_items parent
      WHERE parent.collection_id=g.collection_id AND parent.id=root.parent_item_id
      UNION ALL
      SELECT parent.id,parent.parent_item_id,parent.deleted_at FROM asset_collection_items parent
      JOIN ancestors child ON child.parent_item_id=parent.id WHERE parent.collection_id=g.collection_id
    ) SELECT 1 FROM ancestors WHERE deleted_at IS NOT NULL
  )
  UNION ALL
  SELECT child.id,child.collection_id,child.parent_item_id,child.asset_id,child.node_kind FROM asset_collection_items child
  JOIN readable parent ON child.parent_item_id=parent.id AND child.collection_id=parent.collection_id WHERE child.deleted_at IS NULL
), selected AS (
  SELECT asset_id FROM readable WHERE id=$3 AND node_kind='file'
)
UPDATE assets a SET personal_download_until=GREATEST(a.personal_download_until,$4::timestamptz+interval '10 minutes')
WHERE a.id IN(SELECT asset_id FROM selected) AND a.namespace='presenter.personal'
  AND a.upload_status='completed' AND a.scan_status='clean' AND a.processing_status IN('ready','not_required')
  AND a.deleted_at IS NULL AND a.purged_at IS NULL AND (a.purge_claimed_until IS NULL OR a.purge_claimed_until<$4)
RETURNING a.id`, grant, recipient, item, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", assets.ErrNotFound
	}
	return id, err
}
