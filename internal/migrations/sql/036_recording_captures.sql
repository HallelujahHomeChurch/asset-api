-- Additive progressive intake; complete-inventory packages remain unchanged.
CREATE TABLE recording_captures (
 id text PRIMARY KEY CHECK (id ~ '^[a-zA-Z0-9-]{1,80}$'),
 actor_id text NOT NULL CHECK (actor_id ~ '^[a-zA-Z0-9-]{1,80}$'),
 recording_id text NOT NULL UNIQUE CHECK (recording_id ~ '^[a-zA-Z0-9-]{1,80}$'),
 create_key text NOT NULL UNIQUE CHECK (length(create_key) BETWEEN 1 AND 128),
 state text NOT NULL CHECK (state IN ('uploading','freezing','validating','ready','failed','expired','aborted')),
 created_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL CHECK (expires_at>created_at AND expires_at<=created_at+interval '24 hours'),
 declared_bytes bigint NOT NULL DEFAULT 0 CHECK (declared_bytes BETWEEN 0 AND 10000000000),
 declared_objects integer NOT NULL DEFAULT 0 CHECK (declared_objects BETWEEN 0 AND 10000),
 receipts jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(receipts)='object'),
 package_id text REFERENCES recording_packages(id),
 inventory jsonb,
 terminal_at timestamptz,
 terminal_reason text,
 cleanup_after timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE recording_capture_objects (
 capture_id text NOT NULL REFERENCES recording_captures(id),
 path text NOT NULL CHECK (path ~ '^(master\.m3u8|(480p|720p|1080p)/(index\.m3u8|init\.mp4|seg-[0-9]{6}\.m4s))$'),
 size_bytes bigint NOT NULL CHECK (size_bytes BETWEEN 1 AND 134217728),
 sha256 text NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
 state text NOT NULL CHECK (state IN ('declared','queued','verified','failed')),
 PRIMARY KEY (capture_id,path)
);
CREATE INDEX recording_capture_queue_idx ON recording_capture_objects(capture_id,path) WHERE state='queued';
CREATE INDEX recording_capture_cleanup_idx ON recording_captures(cleanup_after);
