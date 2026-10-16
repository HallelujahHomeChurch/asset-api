-- One bounded recovery for the K4 normal-Stop incident. The original attempts
-- predate the missing End repair; keep their history and every publication field.
UPDATE recording_covers c SET state='pending',attempts=0,next_attempt_at=now()
FROM recording_packages p,recording_captures cap,recording_broadcast_ranges b,recording_live l
WHERE c.package_id='32264350a74315406f3d119a72023cce'
AND c.recording_id='89cf022d-e80e-479f-9bbe-d8590771cc59'
AND ((c.id='40d710ca35646d68c18bd54684693113' AND c.kind='live-auto')
 OR (c.id='ce9d53c03737e84af2b8369acba2a434' AND c.kind='auto'))
AND c.state='failed' AND c.attempts=3 AND c.output_attempt='' AND c.claim_id IS NULL
AND p.id=c.package_id AND p.recording_id=c.recording_id AND p.owner_service='hhc-web-api'
AND p.state='ready' AND p.media_expires_at>now() AND c.expires_at>now()
AND cap.id=p.id AND cap.recording_id=p.recording_id AND cap.state='ready' AND cap.terminal_at IS NULL
AND b.capture_id=p.id AND b.recording_id=p.recording_id AND b.epoch=p.broadcast_epoch
AND NOT b.revoked AND b.start_sequence=0 AND b.end_sequence_exclusive=10
AND l.capture_id=p.id AND l.published_sequence>=9 AND jsonb_array_length(l.segments)>=10
AND NOT EXISTS(SELECT 1 FROM recording_deletions d WHERE d.recording_id=c.recording_id);
