-- Existing work tables are the durable outbox; these rows fence coalesced dispatch.
-- Producers never lock dispatch rows, preserving existing media slot lock ordering.
CREATE TABLE recording_event_wakes (
 lane text PRIMARY KEY CHECK(lane IN ('live','processing','preview')),
 generation bigint NOT NULL DEFAULT 1 CHECK(generation>0),
 dispatch_generation bigint,
 token text,
 state text NOT NULL DEFAULT 'idle' CHECK(state IN ('idle','dispatching','queued','running')),
 lease_until timestamptz,
 next_check_at timestamptz NOT NULL DEFAULT now(),
 last_dispatch_at timestamptz,
 CHECK ((state='idle' AND token IS NULL AND lease_until IS NULL AND dispatch_generation IS NULL)
     OR (state<>'idle' AND token IS NOT NULL AND lease_until IS NOT NULL AND dispatch_generation IS NOT NULL))
);
INSERT INTO recording_event_wakes(lane) VALUES ('live'),('processing'),('preview');
