package postgres

// Recent input, including acceptance of its sealed package, identifies a
// broadcast handoff before the next producer is bound. A silent or abandoned
// capture stops reserving capacity after the existing signal recovery window.
const broadcastDemand = `EXISTS (
 SELECT 1 FROM recording_captures c
 JOIN recording_broadcast_ranges b ON b.capture_id=c.id AND b.epoch=c.broadcast_epoch
 LEFT JOIN recording_packages p ON p.id=c.package_id
 WHERE NOT b.revoked AND c.terminal_at IS NULL AND c.expires_at>now()
 AND c.state IN ('uploading','freezing','validating','ready')
 AND (p.state IS NULL OR p.state NOT IN ('failed','expired'))
 AND GREATEST(c.created_at,c.last_media_confirmed_at,p.completed_at)>now()-interval '5 minutes'
)`

const freeRecordingSlot = `(s.leased_until IS NULL OR s.leased_until<=now())`

// A fixed slot identity prevents racing background transactions from each
// borrowing the remaining live capacity via SKIP LOCKED. If an older claim
// already occupies slot 2, preserve slot 1 when it becomes available.
const backgroundSlotCandidate = `SELECT s.slot FROM recording_processing_slots s
 WHERE ` + freeRecordingSlot + ` AND (NOT ` + broadcastDemand + `
 OR (s.slot=1 AND NOT EXISTS (
 SELECT 1 FROM recording_processing_slots other WHERE other.slot=2
 AND other.leased_until>now() AND other.job_id NOT LIKE 'live:%'
 )))`

const claimBackgroundSlot = backgroundSlotCandidate + ` ORDER BY s.slot FOR UPDATE OF s SKIP LOCKED LIMIT 1`

// Live normally uses slot 2 first. It can use both when there is no queued
// required background work, or when the other slot is occupied by a legacy
// background job. A continuously backlogged second live must give the next
// free general slot to eligible package/source processing.
const claimLiveSlot = `SELECT s.slot FROM recording_processing_slots s WHERE ` + freeRecordingSlot + `
 AND (s.slot=2 OR NOT EXISTS (
 SELECT 1 FROM recording_processing_slots other WHERE other.slot=2
 AND other.leased_until>now() AND other.job_id LIKE 'live:%')
 OR NOT (EXISTS (` + packageWakeCandidate + `) OR EXISTS (` + sourceWakeCandidate + `)))
 ORDER BY s.slot DESC FOR UPDATE OF s SKIP LOCKED LIMIT 1`
