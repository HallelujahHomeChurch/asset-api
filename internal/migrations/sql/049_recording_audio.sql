-- Empty on migration: historical packages require separately approved backfill.
CREATE TABLE recording_audio (
 package_id text PRIMARY KEY REFERENCES recording_packages(id),
 version text NOT NULL CHECK(version='aac-hls-v1'),
 source_prefix text NOT NULL,
 source_digest text NOT NULL,
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','processing','ready','failed')),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 3),
 claim_id text,
 claimed_until timestamptz,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE recording_audio_attempts (
 claim_id text PRIMARY KEY,
 package_id text NOT NULL REFERENCES recording_audio(package_id),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX recording_audio_pending_idx ON recording_audio(next_attempt_at)
 WHERE state IN ('pending','processing');
