ALTER TABLE recording_captures ADD COLUMN broadcast_epoch bigint NOT NULL DEFAULT 0 CHECK (broadcast_epoch BETWEEN 0 AND 9007199254740991);

CREATE TABLE recording_broadcast_ranges (
 capture_id text PRIMARY KEY REFERENCES recording_captures(id),
 recording_id text NOT NULL,
 epoch bigint NOT NULL CHECK (epoch BETWEEN 1 AND 9007199254740991),
 range_revision bigint NOT NULL CHECK (range_revision BETWEEN 1 AND 9007199254740991),
 start_sequence integer CHECK (start_sequence >= 0),
 end_sequence_exclusive integer CHECK (end_sequence_exclusive >= 0),
 revoked boolean NOT NULL DEFAULT false,
 CHECK (end_sequence_exclusive IS NULL OR (start_sequence IS NOT NULL AND end_sequence_exclusive > start_sequence)),
 UNIQUE (recording_id, epoch)
);

-- Preserve legacy uniqueness while retaining terminal B1 epochs for recovery.
ALTER TABLE recording_captures DROP CONSTRAINT recording_captures_recording_id_key;
CREATE UNIQUE INDEX recording_captures_recording_epoch_idx ON recording_captures(recording_id,broadcast_epoch);
ALTER TABLE recording_packages ADD COLUMN broadcast_epoch bigint NOT NULL DEFAULT 0 CHECK (broadcast_epoch BETWEEN 0 AND 9007199254740991);
DROP INDEX recording_packages_recording_idx;
CREATE UNIQUE INDEX recording_packages_recording_epoch_idx ON recording_packages(recording_id,broadcast_epoch);
