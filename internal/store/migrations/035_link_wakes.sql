-- Linked attempts use a device and vendor scoped lane instead of a local PID or path.
CREATE TABLE link_wake_attempt (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id TEXT NOT NULL REFERENCES device(device_id),
    credential_gen INTEGER NOT NULL,
    principal_id INTEGER NOT NULL REFERENCES principal(id),
    binding_id INTEGER NOT NULL REFERENCES binding(id),
    vendor TEXT NOT NULL,
    lane TEXT NOT NULL,
    route TEXT NOT NULL CHECK (json_valid(route)),
    message_ids TEXT NOT NULL CHECK (json_valid(message_ids)),
    event_seq INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL CHECK (state IN ('dispatching', 'submitted', 'uncertain', 'not_submitted')),
    dispatched_at_ms INTEGER NOT NULL,
    submission_seq INTEGER NOT NULL DEFAULT 0,
    activity_seq INTEGER NOT NULL DEFAULT 0,
    uncertain_at_ms INTEGER,
    released_at_ms INTEGER,
    resolved_at_ms INTEGER
);

CREATE UNIQUE INDEX link_wake_attempt_lane ON link_wake_attempt(device_id, vendor, lane)
    WHERE state <> 'not_submitted' AND released_at_ms IS NULL;
CREATE UNIQUE INDEX link_wake_attempt_member ON link_wake_attempt(principal_id)
    WHERE state <> 'not_submitted' AND released_at_ms IS NULL;
CREATE INDEX link_wake_attempt_member_time ON link_wake_attempt(principal_id, dispatched_at_ms);
CREATE INDEX link_wake_attempt_uncertain ON link_wake_attempt(device_id, uncertain_at_ms)
    WHERE state = 'uncertain' AND released_at_ms IS NULL;

ALTER TABLE link_event_stream ADD COLUMN wake_capable INTEGER NOT NULL DEFAULT 0 CHECK (wake_capable IN (0, 1));
