ALTER TABLE assets
  ADD COLUMN IF NOT EXISTS scan_signature_version text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS scan_failure_category text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS scan_event_id text NOT NULL DEFAULT '';

UPDATE assets a
SET scan_event_id = COALESCE((
  SELECT o.event_id
  FROM asset_scan_outbox o
  WHERE o.asset_id = a.id AND o.asset_etag = a.etag
  ORDER BY o.created_at DESC
  LIMIT 1
), '')
WHERE a.scan_status = 'pending' AND a.scan_event_id = '';

ALTER TABLE assets
  ADD CONSTRAINT assets_pending_scan_event_check
  CHECK (scan_status <> 'pending' OR upload_status <> 'completed' OR scan_event_id <> '') NOT VALID;

ALTER TABLE assets VALIDATE CONSTRAINT assets_pending_scan_event_check;

CREATE TABLE IF NOT EXISTS asset_scan_poison_events (
  poison_id text PRIMARY KEY CHECK (poison_id <> ''),
  event_id text NOT NULL DEFAULT '',
  asset_id text NOT NULL DEFAULT '',
  asset_etag text NOT NULL DEFAULT '',
  reason text NOT NULL CHECK (reason <> ''),
  details text NOT NULL DEFAULT '',
  dequeue_count bigint NOT NULL CHECK (dequeue_count >= 0),
  source_message_id text NOT NULL CHECK (source_message_id <> ''),
  body_sha256 text NOT NULL CHECK (body_sha256 ~ '^[0-9a-f]{64}$'),
  created_at timestamptz NOT NULL,
  forwarded_at timestamptz,
  replayed_at timestamptz,
  replay_event_id text NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS asset_scan_poison_event_idx
  ON asset_scan_poison_events(event_id, created_at DESC)
  WHERE replayed_at IS NULL;
