-- Run with psql -X -v ON_ERROR_STOP=1 -f this-file using an approved observer.
-- Inventory only: zero is not Go; retained work requires a verified claim fence.
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;
SET LOCAL statement_timeout = '5s';
SET LOCAL lock_timeout = '250ms';
SET LOCAL idle_in_transaction_session_timeout = '5s';
WITH components(component) AS (VALUES
 ('scan'), ('derivative'), ('upload'), ('scan_outbox'), ('derivative_outbox'),
 ('package'), ('source'), ('package_preview'), ('cover'), ('live_cover'),
 ('capture'), ('capture_objects'), ('live'), ('live_preview'),
 ('package_attempt'), ('source_attempt'), ('live_attempt'), ('slot'),
 ('wake_live'), ('wake_processing'), ('wake_preview')),
work(component, state, lease_until, due_at, expires_at, attempt_started_at) AS (
 SELECT 'scan', CASE WHEN scan_status='failed' THEN 'failed' ELSE 'pending' END,
  scan_claimed_until, NULL::timestamptz, NULL::timestamptz, NULL::timestamptz
 FROM assets WHERE scan_status IN ('pending','failed') AND deleted_at IS NULL AND purged_at IS NULL
 UNION ALL
 SELECT 'derivative', CASE WHEN processing_status='failed' THEN 'failed' ELSE 'pending' END,
  processing_claimed_until, processing_next_attempt_at, NULL, NULL
 FROM assets WHERE processing_status IN ('pending','failed') AND deleted_at IS NULL AND purged_at IS NULL
 UNION ALL
 SELECT 'upload','uploading',NULL,NULL,expires_at,NULL FROM upload_sessions WHERE completed_at IS NULL AND status='created'
 UNION ALL
 SELECT 'scan_outbox','pending',claimed_until,available_at,NULL,NULL FROM asset_scan_outbox WHERE delivered_at IS NULL
 UNION ALL
 SELECT 'derivative_outbox','pending',claimed_until,available_at,NULL,NULL FROM asset_derivative_outbox WHERE delivered_at IS NULL
 UNION ALL
 SELECT 'package',CASE state WHEN 'failed' THEN 'failed' WHEN 'uploading' THEN 'uploading'
  WHEN 'validating' THEN 'processing' ELSE 'pending' END,claimed_until,next_attempt_at,expires_at,NULL
 FROM recording_packages WHERE state IN ('uploading','freezing','validating','failed')
 UNION ALL
 SELECT 'source',CASE state WHEN 'failed' THEN 'failed' WHEN 'uploading' THEN 'uploading'
  WHEN 'processing' THEN 'processing' ELSE 'pending' END,claimed_until,NULL,COALESCE(retry_until,expires_at),NULL
 FROM recording_sources WHERE state IN ('uploading','finalizing','queued','processing','failed')
 UNION ALL
 SELECT 'package_preview',preview_state,preview_claimed_until,preview_next_attempt_at,media_expires_at,NULL
 FROM recording_packages WHERE state='ready' AND preview_state IN ('pending','processing','failed')
 UNION ALL
 SELECT 'cover',state,claimed_until,next_attempt_at,expires_at,NULL
 FROM recording_covers WHERE state IN ('uploading','pending','processing','failed')
 UNION ALL
 SELECT 'live_cover',state,claimed_until,next_attempt_at,NULL,NULL
 FROM recording_live_covers WHERE state IN ('uploading','pending','processing','failed')
 UNION ALL
 SELECT 'capture',CASE WHEN state='failed' THEN 'failed' ELSE 'uploading' END,NULL,NULL,expires_at,NULL
 FROM recording_captures WHERE terminal_at IS NULL OR state='failed'
 UNION ALL
 SELECT 'capture_objects',CASE WHEN o.state='failed' THEN 'failed' ELSE 'pending' END,NULL,NULL,NULL,NULL
 FROM recording_capture_objects o JOIN recording_captures c ON c.id=o.capture_id
 WHERE c.terminal_at IS NULL AND o.state IN ('declared','queued','failed')
 UNION ALL
 SELECT 'live','pending',claimed_until,next_attempt_at,NULL,NULL FROM recording_live
 WHERE claim_id IS NOT NULL OR revision>published_revision OR NOT ended
 UNION ALL
 SELECT 'live_preview','pending',p.claimed_until,p.next_attempt_at,NULL,NULL
 FROM recording_live_previews p JOIN recording_live l ON l.capture_id=p.capture_id
 WHERE p.claim_id IS NOT NULL OR p.completed_sequence<l.published_sequence
 UNION ALL
 SELECT 'package_attempt','review',NULL,NULL,NULL,created_at
 FROM recording_package_attempts WHERE finished_at IS NULL AND state='processing'
 UNION ALL
 SELECT 'source_attempt','review',NULL,NULL,NULL,created_at
 FROM recording_source_attempts WHERE finished_at IS NULL AND state='processing'
 UNION ALL
 SELECT 'live_attempt','review',NULL,NULL,NULL,created_at FROM recording_live_attempts WHERE finished_at IS NULL AND NOT purged
 UNION ALL
 SELECT 'slot','review',leased_until,NULL,NULL,NULL FROM recording_processing_slots WHERE claim_id IS NOT NULL
 UNION ALL
 SELECT 'wake_'||lane,CASE WHEN state='running' THEN 'processing' ELSE 'pending' END,
  lease_until,next_check_at,NULL,NULL FROM recording_event_wakes WHERE state<>'idle'
), classified AS (
 SELECT *, CASE
  WHEN lease_until>transaction_timestamp() THEN 'leased'
  WHEN lease_until IS NOT NULL THEN 'expired_claim_review'
  WHEN state IN ('processing','review') THEN 'unleased_effect_review'
  WHEN state='failed' THEN 'terminal_failed_candidate'
  WHEN state='uploading' THEN 'upload_session_review'
  WHEN due_at>transaction_timestamp() THEN 'retained_delayed_candidate'
  ELSE 'retained_due_candidate' END AS category
 FROM work
)
SELECT c.component,
 count(*) FILTER (WHERE category='leased') AS leased,
 count(*) FILTER (WHERE category='expired_claim_review') AS expired_claim_review,
 count(*) FILTER (WHERE category='unleased_effect_review') AS unleased_effect_review,
 count(*) FILTER (WHERE category='terminal_failed_candidate') AS terminal_failed_candidate,
 count(*) FILTER (WHERE category='upload_session_review') AS upload_session_review,
 count(*) FILTER (WHERE category='retained_delayed_candidate') AS retained_delayed_candidate,
 count(*) FILTER (WHERE category='retained_due_candidate') AS retained_due_candidate,
 max(lease_until) AS max_lease_until, min(due_at) AS next_due_at,
 min(expires_at) AS earliest_expiry_at, max(attempt_started_at) AS last_attempt_started_at,
 transaction_timestamp() AS observed_at
FROM components c LEFT JOIN classified w USING (component)
GROUP BY c.component ORDER BY c.component;
COMMIT;
