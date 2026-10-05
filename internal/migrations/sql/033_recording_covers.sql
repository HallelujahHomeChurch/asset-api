CREATE TABLE recording_covers (
 id text PRIMARY KEY,
 package_id text NOT NULL REFERENCES recording_packages(id),
 recording_id text NOT NULL,
 actor_id text NOT NULL,
 idempotency_key text NOT NULL,
 kind text NOT NULL CHECK(kind IN ('auto','custom')),
 mime text NOT NULL DEFAULT '',
 digest text NOT NULL DEFAULT '',
 state text NOT NULL CHECK(state IN ('uploading','pending','processing','ready','failed','expired')),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 3),
 claim_id text,
 claimed_until timestamptz,
 output_attempt text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 cleanup_after timestamptz NOT NULL DEFAULT now(),
 UNIQUE(recording_id,actor_id,idempotency_key)
);
CREATE UNIQUE INDEX recording_covers_auto ON recording_covers(package_id) WHERE kind='auto';
CREATE INDEX recording_covers_pending ON recording_covers(next_attempt_at) WHERE state IN ('pending','processing');
CREATE TABLE recording_cover_attempts (
 claim_id text PRIMARY KEY,
 cover_id text NOT NULL REFERENCES recording_covers(id),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE recording_cover_references (
 reference_id text PRIMARY KEY,
 cover_id text NOT NULL REFERENCES recording_covers(id),
 released_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now()
);
