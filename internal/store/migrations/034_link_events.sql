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
    PRIMARY KEY (device_id, stream),
    UNIQUE (device_id)
);

CREATE INDEX link_event_stream_age ON link_event_stream(created_at_ms);

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

CREATE TRIGGER device_offer_registration_insert AFTER INSERT ON device_key
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key, membership)
    VALUES (NEW.device_id, NEW.vendor, NEW.key, 1)
    ON CONFLICT(device_id, vendor, key) DO UPDATE SET membership = 1;
END;

CREATE TRIGGER device_offer_registration_update AFTER UPDATE OF leaf_generation, leaf_epoch, credential_gen, state ON device_key
WHEN OLD.leaf_generation <> NEW.leaf_generation OR OLD.leaf_epoch <> NEW.leaf_epoch
  OR OLD.credential_gen <> NEW.credential_gen OR OLD.state <> NEW.state
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key, invalidate_reason, membership)
    VALUES (NEW.device_id, NEW.vendor, NEW.key, 'route', 1)
    ON CONFLICT(device_id, vendor, key) DO UPDATE SET invalidate_reason = 'route', membership = 1;
END;

CREATE TRIGGER device_offer_credential_change AFTER UPDATE OF credential_gen ON device
WHEN OLD.credential_gen <> NEW.credential_gen
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key, invalidate_reason, membership)
    SELECT device_id, vendor, key, 'route', 1 FROM device_key WHERE device_id = NEW.device_id AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO UPDATE SET invalidate_reason = 'route', membership = 1;
END;

CREATE TRIGGER device_offer_selection_insert AFTER INSERT ON member_selection
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key, membership)
    SELECT device_id, vendor, key, 1 FROM device_key WHERE principal_id = NEW.principal_id AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO UPDATE SET membership = 1;
END;

CREATE TRIGGER device_offer_selection_update AFTER UPDATE OF binding_id, generation ON member_selection
WHEN NEW.binding_id <> OLD.binding_id OR NEW.generation <> OLD.generation
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key, invalidate_reason, membership)
    SELECT device_id, vendor, key, 'move', 1 FROM device_key WHERE principal_id = NEW.principal_id AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO UPDATE SET invalidate_reason = 'move', membership = 1;
END;

CREATE TRIGGER device_offer_binding AFTER UPDATE OF state, dnd, route_epoch ON binding
WHEN (OLD.state <> NEW.state OR OLD.dnd <> NEW.dnd OR OLD.route_epoch <> NEW.route_epoch)
  AND EXISTS (SELECT 1 FROM member_selection WHERE principal_id = NEW.principal_id AND binding_id = NEW.id)
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key, invalidate_reason, membership)
    SELECT device_id, vendor, key, CASE
        WHEN NEW.state <> 'live' THEN 'depart'
        WHEN OLD.state <> NEW.state OR OLD.route_epoch <> NEW.route_epoch THEN 'route'
        ELSE 'dnd' END,
        OLD.state <> NEW.state OR OLD.route_epoch <> NEW.route_epoch
    FROM device_key WHERE principal_id = NEW.principal_id AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO UPDATE SET
        invalidate_reason = CASE WHEN device_offer_dirty.invalidate_reason = 'move' THEN 'move' ELSE excluded.invalidate_reason END,
        membership = MAX(device_offer_dirty.membership, excluded.membership);
END;

CREATE TRIGGER device_offer_pending_insert AFTER INSERT ON recipient
WHEN NEW.picked_up_at_ms IS NULL
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key)
    SELECT device_id, vendor, key FROM device_key WHERE principal_id = NEW.principal_id AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO NOTHING;
END;

CREATE TRIGGER device_offer_pickup AFTER UPDATE OF picked_up_at_ms ON recipient
WHEN OLD.picked_up_at_ms IS NOT NEW.picked_up_at_ms
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key)
    SELECT device_id, vendor, key FROM device_key WHERE principal_id = NEW.principal_id AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO NOTHING;
END;

CREATE TRIGGER device_offer_obligation AFTER UPDATE OF state ON obligation
WHEN OLD.state <> NEW.state
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key, invalidate_reason)
    SELECT device_id, vendor, key, CASE WHEN NEW.state = 'cancelled' THEN 'cancel' ELSE '' END
    FROM device_key WHERE principal_id = NEW.principal_id AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO UPDATE SET invalidate_reason =
        COALESCE(NULLIF(device_offer_dirty.invalidate_reason, ''), excluded.invalidate_reason);
END;

CREATE TRIGGER device_offer_policy_insert AFTER INSERT ON adapter_policy
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key, invalidate_reason)
    SELECT device_id, vendor, key, 'policy' FROM device_key WHERE vendor = NEW.adapter AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO UPDATE SET invalidate_reason =
        COALESCE(NULLIF(device_offer_dirty.invalidate_reason, ''), 'policy');
END;

CREATE TRIGGER device_offer_policy_update AFTER UPDATE OF enabled, epoch ON adapter_policy
WHEN OLD.enabled <> NEW.enabled OR OLD.epoch <> NEW.epoch
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key, invalidate_reason)
    SELECT device_id, vendor, key, 'policy' FROM device_key WHERE vendor = NEW.adapter AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO UPDATE SET invalidate_reason =
        COALESCE(NULLIF(device_offer_dirty.invalidate_reason, ''), 'policy');
END;

CREATE TRIGGER device_offer_written AFTER UPDATE OF written_at_ms ON offer
WHEN NEW.device_credential_gen IS NOT NULL AND OLD.written_at_ms IS NULL AND NEW.written_at_ms IS NOT NULL
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key)
    SELECT device_id, vendor, key FROM device_key WHERE principal_id = NEW.principal_id AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO NOTHING;
END;

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
    leaf_seq INTEGER NOT NULL CHECK (leaf_seq > 0),
    kind TEXT NOT NULL,
    payload TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('applied', 'stale', 'quarantined')),
    code TEXT NOT NULL,
    recorded_at_ms INTEGER NOT NULL,
    PRIMARY KEY (device_id, credential_gen, id)
);
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
