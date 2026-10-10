ALTER TABLE recording_broadcast_ranges ADD COLUMN member_state text NOT NULL DEFAULT 'blocked' CHECK (member_state IN ('blocked','live','vod'));
