-- The database owns media work; these three rows coalesce transactional wakeups.
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

CREATE FUNCTION recording_event_wake() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE lane_name text;
BEGIN
 -- Identical lock ordering prevents multi-table state transitions deadlocking.
 FOREACH lane_name IN ARRAY ARRAY['live','processing','preview'] LOOP
  UPDATE recording_event_wakes SET generation=generation+1,next_check_at=LEAST(next_check_at,clock_timestamp()) WHERE lane=lane_name;
 END LOOP;
 RETURN NULL;
END $$;

CREATE TRIGGER recording_packages_wake AFTER INSERT OR UPDATE OR DELETE ON recording_packages FOR EACH STATEMENT EXECUTE FUNCTION recording_event_wake();
CREATE TRIGGER recording_sources_wake AFTER INSERT OR UPDATE OR DELETE ON recording_sources FOR EACH STATEMENT EXECUTE FUNCTION recording_event_wake();
CREATE TRIGGER recording_live_wake AFTER INSERT OR UPDATE OR DELETE ON recording_live FOR EACH STATEMENT EXECUTE FUNCTION recording_event_wake();
CREATE TRIGGER recording_objects_wake AFTER INSERT OR UPDATE OR DELETE ON recording_capture_objects FOR EACH STATEMENT EXECUTE FUNCTION recording_event_wake();
CREATE TRIGGER recording_captures_wake AFTER INSERT OR UPDATE OR DELETE ON recording_captures FOR EACH STATEMENT EXECUTE FUNCTION recording_event_wake();
CREATE TRIGGER recording_ranges_wake AFTER INSERT OR UPDATE OR DELETE ON recording_broadcast_ranges FOR EACH STATEMENT EXECUTE FUNCTION recording_event_wake();
CREATE TRIGGER recording_live_covers_wake AFTER INSERT OR UPDATE OR DELETE ON recording_live_covers FOR EACH STATEMENT EXECUTE FUNCTION recording_event_wake();
CREATE TRIGGER recording_covers_wake AFTER INSERT OR UPDATE OR DELETE ON recording_covers FOR EACH STATEMENT EXECUTE FUNCTION recording_event_wake();
CREATE TRIGGER recording_live_previews_wake AFTER INSERT OR UPDATE OR DELETE ON recording_live_previews FOR EACH STATEMENT EXECUTE FUNCTION recording_event_wake();
