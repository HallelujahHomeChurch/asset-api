-- Existing captures intentionally retain legacy recovery behavior.
ALTER TABLE recording_captures
 ADD COLUMN recovery_version text NOT NULL DEFAULT '' CHECK (recovery_version IN ('', 'common-watermark-v1')),
 ADD COLUMN recovery_active boolean NOT NULL DEFAULT false,
 ADD COLUMN recovery_activation_at timestamptz,
 ADD COLUMN recovery_baseline_end integer NOT NULL DEFAULT 0,
 ADD COLUMN recovery_segment_ns bigint NOT NULL DEFAULT 0,
 ADD COLUMN recovery_common_end integer NOT NULL DEFAULT 0,
 ADD COLUMN recovery_common_accepted_at timestamptz,
 ADD COLUMN recovery_deadline timestamptz;
ALTER TABLE recording_captures ADD CONSTRAINT recording_recovery_shape CHECK (
 recovery_baseline_end BETWEEN 0 AND 4320
 AND recovery_common_end BETWEEN recovery_baseline_end AND 4320
 AND recovery_segment_ns IN (0,10010000000,30000000000,30030000000)
 AND (NOT recovery_active OR (
  recovery_version='common-watermark-v1' AND recovery_activation_at IS NOT NULL
  AND recovery_baseline_end>0 AND recovery_segment_ns>0
  AND recovery_common_accepted_at IS NOT NULL AND recovery_deadline IS NOT NULL
 ))
);
