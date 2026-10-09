CREATE TABLE recording_live_covers (
 id text PRIMARY KEY,
 scope text NOT NULL,
 recording_id text NOT NULL DEFAULT '',
 actor_id text NOT NULL,
 idempotency_key text NOT NULL,
 kind text NOT NULL CHECK(kind IN ('auto','custom')),
 mime text NOT NULL DEFAULT '',
 digest text NOT NULL DEFAULT '',
 output_digest text NOT NULL DEFAULT '',
 state text NOT NULL CHECK(state IN ('uploading','pending','processing','ready','failed','expired')),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 3),
 claim_id text,
 claimed_until timestamptz,
 output_attempt text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 cleanup_after timestamptz NOT NULL DEFAULT now(),
 UNIQUE(scope,actor_id,idempotency_key),
 CHECK((scope='defaults' AND recording_id='' AND kind='custom') OR (scope<>'defaults' AND recording_id<>''))
);
CREATE UNIQUE INDEX recording_live_covers_auto ON recording_live_covers(scope) WHERE kind='auto';
CREATE TABLE recording_live_cover_references (
 reference_id text PRIMARY KEY,
 cover_id text NOT NULL REFERENCES recording_live_covers(id),
 scope text NOT NULL,
 recording_id text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 released_at timestamptz
);
CREATE TABLE recording_live_cover_attempts (
 claim_id text PRIMARY KEY,
 cover_id text NOT NULL REFERENCES recording_live_covers(id),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX recording_live_covers_pending ON recording_live_covers(next_attempt_at) WHERE state IN ('pending','processing');

ALTER TABLE recording_covers DROP CONSTRAINT recording_covers_kind_check;
ALTER TABLE recording_covers ADD CONSTRAINT recording_covers_kind_check CHECK(kind IN ('auto','custom','live-auto'));
ALTER TABLE recording_covers ADD COLUMN inherited_cover_id text REFERENCES recording_live_covers(id);
CREATE UNIQUE INDEX recording_covers_live_auto ON recording_covers(package_id) WHERE kind='live-auto';
