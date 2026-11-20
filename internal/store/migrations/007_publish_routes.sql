ALTER TABLE binding ADD COLUMN route_epoch INTEGER NOT NULL DEFAULT 1 CHECK (route_epoch >= 1);

CREATE TABLE publish_route (
    binding_id INTEGER PRIMARY KEY REFERENCES binding(id),
    principal_id INTEGER NOT NULL REFERENCES principal(id),
    binding_epoch INTEGER NOT NULL DEFAULT 1 CHECK (binding_epoch >= 1),
    generation INTEGER NOT NULL CHECK (generation >= 1),
    adapter TEXT NOT NULL,
    session_ref TEXT NOT NULL,
    launch_ref TEXT NOT NULL,
    launch_verified INTEGER NOT NULL CHECK (launch_verified IN (0, 1)),
    policy_epoch INTEGER NOT NULL CHECK (policy_epoch >= 0),
    mode TEXT NOT NULL,
    mode_at_ms INTEGER NOT NULL DEFAULT 0,
    socket_path TEXT,
    thread_id TEXT,
    watermark_message_id INTEGER NOT NULL CHECK (watermark_message_id >= 0),
    state TEXT NOT NULL CHECK (state IN ('active', 'ended', 'invalidated')),
    reason TEXT NOT NULL DEFAULT '',
    registered_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL
);

CREATE INDEX publish_route_principal ON publish_route(principal_id);
