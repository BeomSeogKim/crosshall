CREATE TABLE link_event_cursor (
    device_id TEXT PRIMARY KEY REFERENCES device(device_id) ON DELETE CASCADE,
    head INTEGER NOT NULL DEFAULT 0,
    acked INTEGER NOT NULL DEFAULT 0,
    trimmed INTEGER NOT NULL DEFAULT 0,
    boot TEXT NOT NULL DEFAULT '',
    credential_gen INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE device_event (
    device_id TEXT NOT NULL REFERENCES link_event_cursor(device_id) ON DELETE CASCADE,
    seq INTEGER NOT NULL,
    kind TEXT NOT NULL,
    body TEXT NOT NULL CHECK (json_valid(body)),
    at_ms INTEGER NOT NULL,
    PRIMARY KEY (device_id, seq)
);

CREATE INDEX device_event_age ON device_event(at_ms);

CREATE TRIGGER device_event_trim
AFTER DELETE ON device_event
BEGIN
    UPDATE link_event_cursor SET trimmed = max(trimmed, OLD.seq) WHERE device_id = OLD.device_id;
END;

CREATE TABLE link_event_stream (
    device_id TEXT NOT NULL REFERENCES link_event_cursor(device_id) ON DELETE CASCADE,
    stream TEXT NOT NULL,
    admission_gen INTEGER NOT NULL,
    boot TEXT NOT NULL,
    snapshot_seq INTEGER,
    ack_seq INTEGER,
    created_at_ms INTEGER NOT NULL,
    last_ack_at_ms INTEGER,
    PRIMARY KEY (device_id, stream),
    UNIQUE (device_id)
);

CREATE INDEX link_event_stream_age ON link_event_stream(COALESCE(last_ack_at_ms, created_at_ms));

CREATE TABLE link_event_snapshot (
    device_id TEXT NOT NULL,
    stream TEXT NOT NULL,
    part INTEGER NOT NULL,
    body TEXT NOT NULL CHECK (json_valid(body)),
    PRIMARY KEY (device_id, stream, part),
    FOREIGN KEY (device_id, stream) REFERENCES link_event_stream(device_id, stream) ON DELETE CASCADE
);

CREATE TABLE link_event_cut_stage (
    device_id TEXT NOT NULL,
    stream TEXT NOT NULL,
    seq INTEGER NOT NULL,
    vendor TEXT NOT NULL,
    key TEXT NOT NULL,
    body TEXT NOT NULL CHECK (json_valid(body)),
    PRIMARY KEY (device_id, stream, seq, vendor, key),
    FOREIGN KEY (device_id, stream) REFERENCES link_event_stream(device_id, stream) ON DELETE CASCADE
);

CREATE TABLE link_event_cut (
    device_id TEXT NOT NULL REFERENCES link_event_cursor(device_id) ON DELETE CASCADE,
    vendor TEXT NOT NULL,
    key TEXT NOT NULL,
    boot TEXT NOT NULL,
    leaf_generation TEXT NOT NULL,
    binding_id INTEGER NOT NULL,
    membership_generation INTEGER NOT NULL,
    route_epoch INTEGER NOT NULL,
    leaf_seq INTEGER NOT NULL,
    absent INTEGER NOT NULL CHECK (absent IN (0, 1)),
    event_seq INTEGER NOT NULL,
    PRIMARY KEY (device_id, vendor, key)
);

ALTER TABLE offer ADD COLUMN device_credential_gen INTEGER;
ALTER TABLE offer ADD COLUMN device_membership_gen INTEGER;

CREATE TABLE device_offer_dirty (
    device_id TEXT NOT NULL REFERENCES device(device_id) ON DELETE CASCADE,
    vendor TEXT NOT NULL,
    key TEXT NOT NULL,
    invalidate_reason TEXT NOT NULL DEFAULT '',
    membership INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (device_id, vendor, key)
) WITHOUT ROWID;

CREATE TABLE device_offer (
    device_id TEXT NOT NULL REFERENCES device(device_id) ON DELETE CASCADE,
    vendor TEXT NOT NULL,
    key TEXT NOT NULL,
    offer_id INTEGER NOT NULL REFERENCES offer(id),
    expires_at_ms INTEGER NOT NULL,
    body TEXT NOT NULL,
    PRIMARY KEY (device_id, vendor, key)
) WITHOUT ROWID;
CREATE INDEX device_offer_expiry ON device_offer(device_id, expires_at_ms);

INSERT INTO device_offer_dirty(device_id, vendor, key, membership)
SELECT device_id, vendor, key, 1 FROM device_key WHERE state = 'live'
ON CONFLICT(device_id, vendor, key) DO NOTHING;


CREATE TABLE link_event_input (
    device_id TEXT NOT NULL REFERENCES device(device_id),
    credential_gen INTEGER NOT NULL,
    id INTEGER NOT NULL CHECK (id > 0),
    vendor TEXT NOT NULL,
    key TEXT NOT NULL,
    boot TEXT NOT NULL,
    leaf_generation TEXT NOT NULL,
    leaf_seq INTEGER NOT NULL CHECK (leaf_seq > 0 OR (leaf_seq = 0 AND status = 'stale')),
    kind TEXT NOT NULL,
    payload TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('applied', 'stale', 'quarantined')),
    code TEXT NOT NULL,
    recorded_at_ms INTEGER NOT NULL,
    PRIMARY KEY (device_id, credential_gen, boot, id)
);
CREATE INDEX link_event_input_age ON link_event_input(recorded_at_ms);
CREATE INDEX link_event_input_pending ON link_event_input(device_id, credential_gen, vendor, key, leaf_seq)
    WHERE status = 'quarantined';

CREATE TABLE link_event_fact_head (
    device_id TEXT NOT NULL REFERENCES device(device_id),
    vendor TEXT NOT NULL,
    key TEXT NOT NULL,
    credential_gen INTEGER NOT NULL,
    boot TEXT NOT NULL,
    leaf_generation TEXT NOT NULL,
    binding_id INTEGER NOT NULL,
    membership_generation INTEGER NOT NULL,
    route_epoch INTEGER NOT NULL,
    leaf_seq INTEGER NOT NULL,
    effective_at_ms INTEGER NOT NULL,
    quarantined INTEGER NOT NULL CHECK (quarantined IN (0, 1)),
    PRIMARY KEY (device_id, vendor, key)
);

CREATE TABLE link_event_launch (
    principal_id INTEGER PRIMARY KEY REFERENCES principal(id),
    device_id TEXT NOT NULL REFERENCES device(device_id),
    credential_gen INTEGER NOT NULL,
    boot TEXT NOT NULL,
    leaf_generation TEXT NOT NULL,
    binding_id INTEGER NOT NULL,
    membership_generation INTEGER NOT NULL,
    route_epoch INTEGER NOT NULL,
    launch_ref TEXT NOT NULL,
    projection_ref TEXT NOT NULL,
    policy_epoch INTEGER NOT NULL,
    verified INTEGER NOT NULL CHECK (verified IN (0, 1)),
    hosted INTEGER NOT NULL CHECK (hosted IN (0, 1))
);
