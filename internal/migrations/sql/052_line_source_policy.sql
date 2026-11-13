-- No existing pair changes semantics. The new producer remains configuration-gated.
ALTER TABLE assets ADD COLUMN source_policy jsonb;
ALTER TABLE assets ADD COLUMN source_expires_at timestamptz;
ALTER TABLE assets DROP CONSTRAINT assets_scan_status_check;
ALTER TABLE assets ADD CONSTRAINT assets_scan_status_check
 CHECK (scan_status IN ('pending','clean','infected','failed','not_required')) NOT VALID;
ALTER TABLE assets ADD CONSTRAINT assets_line_source_policy_check CHECK (
 CASE WHEN security_profile='line-source-unscanned-v1' THEN COALESCE(
  owner_service='hhc-line-harness' AND namespace IN ('line.group.file','line.group.media-sync')
  AND processing_profile='no-processing-v1' AND processing_status='not_required' AND visibility='restricted'
  AND source_policy->>'version'='1'
  AND source_policy->>'ref' ~ '^[A-Za-z0-9_-]{12,128}$'
  AND source_policy->>'revision' ~ '^[A-Za-z0-9_-]{12,128}$'
  AND source_expires_at=(source_policy->>'expiresAt')::timestamptz
  AND source_expires_at=(source_policy->>'publishedAt')::timestamptz + interval '2160 hours'
  AND scan_status IN ('pending','not_required')
  AND (upload_status<>'completed' OR scan_status='not_required')
  AND scan_signature_version='' AND derivation_proof IS NULL,
 false)
 ELSE source_policy IS NULL AND source_expires_at IS NULL AND scan_status<>'not_required' END
) NOT VALID;
CREATE INDEX assets_line_source_expiry_idx ON assets(source_expires_at,id)
 WHERE source_expires_at IS NOT NULL AND purged_at IS NULL;
