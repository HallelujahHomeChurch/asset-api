-- Authorized Asset DB operator only; no credentials or object URLs are returned.
-- This file reads exactly the two failed captures. It does not recover either.
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;
SET LOCAL statement_timeout = '10s';
SET LOCAL lock_timeout = '3s';
SELECT clock_timestamp() AS observed_at;
SELECT id,recording_id,state,created_at,expires_at,terminal_at,terminal_reason,
       segment_frames,broadcast_epoch,package_id,declared_objects,declared_bytes,
       idle_end_sequence_exclusive,last_media_confirmed_at,cleanup_after,
       inventory->>'inventoryDigest' AS inventory_digest,
       inventory->'renditions' AS renditions
FROM recording_captures
WHERE id IN ('fcdd4c04a9db64bdb6d0dff27b4c0f0f','d62dc3154bbffc779f88480351e9812c');
SELECT c.id, r.key AS operation_key, r.value AS stored_receipt
FROM recording_captures c CROSS JOIN LATERAL jsonb_each(c.receipts) r
WHERE c.id IN ('fcdd4c04a9db64bdb6d0dff27b4c0f0f','d62dc3154bbffc779f88480351e9812c')
  AND (r.key IN ('55408f45-74ce-4865-8c5a-f5396a113d89.declare-941b859536ff4606ec60d8a68d55ceec79637eb9','105f88f4-f976-447f-9a5a-7ddd4980e41c.seal')
       OR r.value->'Receipt'->>'operation' IN ('seal','abort'));
SELECT capture_id,path,size_bytes,sha256,state
FROM recording_capture_objects
WHERE capture_id IN ('fcdd4c04a9db64bdb6d0dff27b4c0f0f','d62dc3154bbffc779f88480351e9812c')
ORDER BY capture_id,path;
SELECT capture_id,revision,published_revision,published_sequence,published_media_end,
       last_advanced_at,ended,ended_at,claim_id,claimed_until,next_attempt_at,
       attempts,read_grant_until,cleanup_after,jsonb_array_length(segments) AS segment_count,
       segments->(jsonb_array_length(segments)-1) AS last_validated_segment
FROM recording_live
WHERE capture_id IN ('fcdd4c04a9db64bdb6d0dff27b4c0f0f','d62dc3154bbffc779f88480351e9812c');
SELECT id,state,validation_attempts,validation_error,next_attempt_at,claim_id,
       claimed_until,ready_at,expires_at,cleanup_after,processing_progress
FROM recording_packages
WHERE id IN (SELECT package_id FROM recording_captures WHERE id IN ('fcdd4c04a9db64bdb6d0dff27b4c0f0f','d62dc3154bbffc779f88480351e9812c'));
SELECT package_id,claim_id,state,created_at,finished_at
FROM recording_package_attempts
WHERE package_id IN (SELECT package_id FROM recording_captures WHERE id IN ('fcdd4c04a9db64bdb6d0dff27b4c0f0f','d62dc3154bbffc779f88480351e9812c'));
SELECT slot,job_id,claim_id,leased_until FROM recording_processing_slots
WHERE job_id IN ('fcdd4c04a9db64bdb6d0dff27b4c0f0f','live:fcdd4c04a9db64bdb6d0dff27b4c0f0f','d62dc3154bbffc779f88480351e9812c','live:d62dc3154bbffc779f88480351e9812c');
ROLLBACK;
