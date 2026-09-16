UPDATE asset_grants AS g
SET revoked_at = now()
FROM assets AS a
WHERE g.asset_id = a.id
  AND a.namespace = 'cms.weekly.pdf'
  AND g.subject_type = 'public'
  AND g.revoked_at IS NULL;

UPDATE assets
SET visibility = 'private', updated_at = now()
WHERE namespace = 'cms.weekly.pdf'
  AND visibility <> 'private';
