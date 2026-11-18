ALTER TABLE observation ADD COLUMN launch_ref TEXT;
ALTER TABLE observation ADD COLUMN event_ref TEXT;
ALTER TABLE observation ADD COLUMN turn_ref TEXT;
ALTER TABLE observation ADD COLUMN hook_at_ms INTEGER;
ALTER TABLE observation ADD COLUMN profile TEXT;

CREATE UNIQUE INDEX observation_event ON observation(principal_id, launch_ref, event_ref)
    WHERE event_ref IS NOT NULL;

ALTER TABLE binding ADD COLUMN roster_baseline_seq INTEGER CHECK (roster_baseline_seq >= 0);
ALTER TABLE binding ADD COLUMN departed_by_hook INTEGER NOT NULL DEFAULT 0 CHECK (departed_by_hook IN (0, 1));

CREATE TABLE adapter_policy (
    adapter TEXT NOT NULL PRIMARY KEY,
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    reminder INTEGER NOT NULL CHECK (reminder IN (0, 1)),
    epoch INTEGER NOT NULL CHECK (epoch >= 0),
    changed_at_ms INTEGER NOT NULL
);

CREATE TABLE adapter_launch (
    principal_id INTEGER NOT NULL REFERENCES principal(id),
    launch_ref TEXT NOT NULL,
    profile TEXT NOT NULL,
    verified INTEGER NOT NULL CHECK (verified IN (0, 1)),
    started_hook_at_ms INTEGER NOT NULL,
    ended_hook_at_ms INTEGER,
    PRIMARY KEY (principal_id, launch_ref)
);

CREATE TABLE offer (
    id INTEGER PRIMARY KEY,
    principal_id INTEGER NOT NULL REFERENCES principal(id),
    binding_id INTEGER NOT NULL REFERENCES binding(id),
    generation TEXT NOT NULL,
    launch_ref TEXT,
    epoch INTEGER NOT NULL CHECK (epoch >= 0),
    reason TEXT NOT NULL CHECK (reason IN ('start', 'prompt', 'compact')),
    evidence TEXT NOT NULL,
    at_ms INTEGER NOT NULL,
    UNIQUE (id, principal_id)
);

CREATE TABLE offer_item (
    offer_id INTEGER NOT NULL,
    message_id INTEGER NOT NULL,
    principal_id INTEGER NOT NULL,
    PRIMARY KEY (offer_id, message_id),
    FOREIGN KEY (offer_id, principal_id) REFERENCES offer(id, principal_id),
    FOREIGN KEY (message_id, principal_id) REFERENCES recipient(message_id, principal_id)
);

CREATE INDEX offer_item_recipient ON offer_item(principal_id, message_id);

CREATE TABLE turn (
    principal_id INTEGER NOT NULL,
    launch_ref TEXT NOT NULL,
    turn_ref TEXT NOT NULL,
    opened_hook_at_ms INTEGER NOT NULL,
    finishing_hook_at_ms INTEGER,
    superseded INTEGER NOT NULL DEFAULT 0 CHECK (superseded IN (0, 1)),
    reminder_claimed_at_ms INTEGER,
    PRIMARY KEY (principal_id, launch_ref, turn_ref),
    FOREIGN KEY (principal_id, launch_ref) REFERENCES adapter_launch(principal_id, launch_ref)
);

CREATE TABLE pickup_receipt (
    token_hash TEXT NOT NULL PRIMARY KEY,
    principal_id INTEGER NOT NULL REFERENCES principal(id),
    generation TEXT NOT NULL,
    read_op_key TEXT NOT NULL,
    issued_at_ms INTEGER NOT NULL,
    expires_at_ms INTEGER NOT NULL,
    used_at_ms INTEGER,
    turn_ref TEXT,
    tool_call_ref TEXT,
    UNIQUE (principal_id, read_op_key)
);

CREATE TABLE pickup_receipt_item (
    token_hash TEXT NOT NULL REFERENCES pickup_receipt(token_hash),
    message_id INTEGER NOT NULL REFERENCES message(id),
    PRIMARY KEY (token_hash, message_id)
);

CREATE TABLE capability_evidence (
    vendor TEXT NOT NULL,
    vendor_version TEXT NOT NULL,
    platform TEXT NOT NULL,
    mode TEXT NOT NULL,
    manifest_rev TEXT NOT NULL,
    cap TEXT NOT NULL CHECK (cap IN ('enroll', 'observe', 'boundary', 'reminder')),
    level TEXT NOT NULL CHECK (level IN ('tested', 'candidate', 'unavailable')),
    result_ref TEXT NOT NULL,
    checked_at_ms INTEGER NOT NULL,
    PRIMARY KEY (vendor, vendor_version, platform, mode, manifest_rev, cap)
);

-- The dispatcher publishes changed sequences and clears these flags before
-- committing the same transaction. Upserts preserve the other output class.
CREATE TABLE adapter_dirty (
    principal_id INTEGER PRIMARY KEY REFERENCES principal(id) ON DELETE CASCADE,
    notice INTEGER NOT NULL DEFAULT 0 CHECK (notice IN (0, 1)),
    reminder INTEGER NOT NULL DEFAULT 0 CHECK (reminder IN (0, 1))
);

CREATE TRIGGER adapter_dirty_recipient AFTER INSERT ON recipient
WHEN NEW.picked_up_at_ms IS NULL AND EXISTS (
    SELECT 1 FROM adapter_launch launch
    WHERE launch.principal_id = NEW.principal_id AND (
        launch.ended_hook_at_ms IS NULL
        OR (launch.launch_ref = 'session' AND launch.verified = 0
            AND EXISTS (
                SELECT 1 FROM binding
                WHERE principal_id = launch.principal_id AND state = 'live'
            )
            AND NOT EXISTS (
                SELECT 1 FROM adapter_launch newer
                WHERE newer.principal_id = launch.principal_id
                  AND (newer.started_hook_at_ms, newer.rowid) > (launch.started_hook_at_ms, launch.rowid)
            )
        )
    )
)
BEGIN
    INSERT INTO adapter_dirty(principal_id, notice) VALUES (NEW.principal_id, 1)
    ON CONFLICT(principal_id) DO UPDATE SET notice = 1;
END;

CREATE TRIGGER adapter_dirty_launch_insert AFTER INSERT ON adapter_launch
WHEN NEW.ended_hook_at_ms IS NULL
BEGIN
    INSERT INTO adapter_dirty(principal_id, notice) VALUES (NEW.principal_id, 1)
    ON CONFLICT(principal_id) DO UPDATE SET notice = 1;
END;

CREATE TRIGGER adapter_dirty_launch_update AFTER UPDATE ON adapter_launch
WHEN NEW.ended_hook_at_ms IS NULL AND (
    OLD.ended_hook_at_ms IS NOT NULL OR NEW.started_hook_at_ms <> OLD.started_hook_at_ms
    OR NEW.profile <> OLD.profile OR NEW.verified <> OLD.verified
)
BEGIN
    INSERT INTO adapter_dirty(principal_id, notice) VALUES (NEW.principal_id, 1)
    ON CONFLICT(principal_id) DO UPDATE SET notice = 1;
END;

CREATE TRIGGER adapter_dirty_launch_delete AFTER DELETE ON adapter_launch
WHEN EXISTS (
    SELECT 1 FROM adapter_launch WHERE principal_id = OLD.principal_id AND ended_hook_at_ms IS NULL
)
BEGIN
    INSERT INTO adapter_dirty(principal_id, notice) VALUES (OLD.principal_id, 1)
    ON CONFLICT(principal_id) DO UPDATE SET notice = 1;
END;

CREATE TRIGGER adapter_dirty_binding_insert AFTER INSERT ON binding
WHEN NEW.state = 'live' AND NEW.dnd = 0 AND EXISTS (
    SELECT 1 FROM adapter_launch WHERE principal_id = NEW.principal_id AND ended_hook_at_ms IS NULL
)
BEGIN
    INSERT INTO adapter_dirty(principal_id, notice) VALUES (NEW.principal_id, 1)
    ON CONFLICT(principal_id) DO UPDATE SET notice = 1;
END;

CREATE TRIGGER adapter_dirty_binding_update AFTER UPDATE OF state, dnd ON binding
WHEN NEW.state = 'live' AND NEW.dnd = 0 AND (OLD.state <> 'live' OR OLD.dnd = 1)
    AND EXISTS (
        SELECT 1 FROM adapter_launch WHERE principal_id = NEW.principal_id AND ended_hook_at_ms IS NULL
    )
BEGIN
    INSERT INTO adapter_dirty(principal_id, notice) VALUES (NEW.principal_id, 1)
    ON CONFLICT(principal_id) DO UPDATE SET notice = 1;
END;

CREATE TRIGGER adapter_dirty_policy_insert AFTER INSERT ON adapter_policy
BEGIN
    INSERT INTO adapter_dirty(principal_id, notice, reminder)
    SELECT DISTINCT al.principal_id, 1, 1 FROM adapter_launch al
    JOIN binding b ON b.principal_id = al.principal_id AND b.state = 'live'
    WHERE al.ended_hook_at_ms IS NULL
      AND CASE WHEN json_valid(al.profile) THEN json_extract(al.profile, '$.adapter') END = NEW.adapter
    ON CONFLICT(principal_id) DO UPDATE SET notice = 1, reminder = 1;
END;

CREATE TRIGGER adapter_dirty_policy_update AFTER UPDATE OF enabled, reminder, epoch ON adapter_policy
WHEN NEW.epoch <> OLD.epoch OR NEW.enabled <> OLD.enabled OR NEW.reminder <> OLD.reminder
BEGIN
    INSERT INTO adapter_dirty(principal_id, notice, reminder)
    SELECT DISTINCT al.principal_id, 1, 1 FROM adapter_launch al
    JOIN binding b ON b.principal_id = al.principal_id AND b.state = 'live'
    WHERE al.ended_hook_at_ms IS NULL
      AND CASE WHEN json_valid(al.profile) THEN json_extract(al.profile, '$.adapter') END = NEW.adapter
    ON CONFLICT(principal_id) DO UPDATE SET notice = 1, reminder = 1;
END;
