-- Retry only ready-media derivatives whose old sample landed on a rounded boundary.
-- The migration ledger makes this one retry budget; immutable attempt rows remain.
UPDATE recording_covers c SET state='pending',attempts=0,next_attempt_at=now()
FROM recording_packages p,recording_broadcast_ranges b,recording_live l
WHERE p.id=c.package_id AND b.capture_id=p.id AND l.capture_id=p.id AND c.state='failed' AND c.kind IN('auto','live-auto') AND c.attempts=3 AND c.output_attempt='' AND c.claim_id IS NULL
AND p.state='ready' AND p.media_expires_at>now() AND c.expires_at>now() AND NOT b.revoked AND b.end_sequence_exclusive>b.start_sequence
AND NOT EXISTS(SELECT 1 FROM recording_deletions d WHERE d.recording_id=c.recording_id)
AND EXISTS (SELECT 1 FROM jsonb_array_elements(l.segments) s
WHERE (s->>'sequence')::int>=b.start_sequence AND (s->>'sequence')::int<b.end_sequence_exclusive-1
AND EXISTS(SELECT 1 FROM unnest(ARRAY[0.2::float8,0.5::float8,0.8::float8]) sample
WHERE abs((s->'renditions'->'480p'->>'end')::float8 -
((l.segments->b.start_sequence->'renditions'->'480p'->>'start')::float8+sample*((l.segments->(b.end_sequence_exclusive-1)->'renditions'->'480p'->>'end')::float8-(l.segments->b.start_sequence->'renditions'->'480p'->>'start')::float8)))<1e-9));
