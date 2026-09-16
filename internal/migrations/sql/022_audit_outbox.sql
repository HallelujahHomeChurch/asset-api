CREATE TABLE audit_outboxes (
    id uuid PRIMARY KEY,
    event_id text NOT NULL UNIQUE,
    payload jsonb NOT NULL,
    payload_hash text NOT NULL,
    status text NOT NULL CHECK (status IN ('pending', 'processing', 'delivered', 'dead_letter')),
    attempts integer NOT NULL DEFAULT 0,
    available_at timestamptz NOT NULL,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE INDEX audit_outboxes_delivery_idx ON audit_outboxes (available_at, id)
WHERE status IN ('pending', 'processing');
