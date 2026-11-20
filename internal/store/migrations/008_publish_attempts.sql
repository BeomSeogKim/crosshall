CREATE TABLE announcement (
    principal_id INTEGER NOT NULL,
    message_id INTEGER NOT NULL,
    via TEXT NOT NULL CHECK (via IN ('turn_end', 'publish')),
    ref INTEGER NOT NULL,
    at_ms INTEGER NOT NULL,
    PRIMARY KEY (principal_id, message_id),
    FOREIGN KEY (message_id, principal_id) REFERENCES recipient(message_id, principal_id)
);

INSERT INTO announcement (principal_id, message_id, via, ref, at_ms)
SELECT i.principal_id, i.message_id, 'turn_end', i.notice_id, n.at_ms
FROM turn_end_notice_item i JOIN turn_end_notice n ON n.id = i.notice_id;

DROP TABLE turn_end_notice_item;

ALTER TABLE binding ADD COLUMN publish_quiet INTEGER NOT NULL DEFAULT 0 CHECK (publish_quiet IN (0, 1));

CREATE TABLE publish_route_proof (
    binding_id INTEGER PRIMARY KEY REFERENCES publish_route(binding_id),
    generation INTEGER NOT NULL CHECK (generation >= 1),
    process_pid INTEGER NOT NULL CHECK (process_pid > 0),
    process_start_ms INTEGER NOT NULL CHECK (process_start_ms > 0)
);

CREATE TABLE publish_attempt (
    id INTEGER PRIMARY KEY,
    binding_id INTEGER NOT NULL REFERENCES binding(id),
    principal_id INTEGER NOT NULL REFERENCES principal(id),
    binding_epoch INTEGER NOT NULL CHECK (binding_epoch >= 1),
    generation INTEGER NOT NULL CHECK (generation >= 1),
    adapter TEXT NOT NULL,
    session_ref TEXT NOT NULL,
    launch_ref TEXT NOT NULL,
    policy_epoch INTEGER NOT NULL CHECK (policy_epoch >= 0),
    process_pid INTEGER NOT NULL CHECK (process_pid > 0),
    process_start_ms INTEGER NOT NULL CHECK (process_start_ms > 0),
    state TEXT NOT NULL CHECK (state IN ('reserved', 'submitting', 'not_submitted', 'submitted', 'uncertain')),
    reserved_at_ms INTEGER NOT NULL,
    submitting_at_ms INTEGER,
    resolved_at_ms INTEGER,
    observed_at_ms INTEGER,
    observation TEXT CHECK (observation IN ('own_turn', 'absorbed', 'blocked', 'unknown')),
    retired_at_ms INTEGER,
    UNIQUE (id, principal_id)
);

CREATE UNIQUE INDEX publish_attempt_binding ON publish_attempt(binding_id)
    WHERE observed_at_ms IS NULL AND retired_at_ms IS NULL AND state <> 'not_submitted';

CREATE UNIQUE INDEX publish_attempt_queue ON publish_attempt(process_pid, process_start_ms)
    WHERE observed_at_ms IS NULL AND retired_at_ms IS NULL AND state <> 'not_submitted';

CREATE INDEX publish_attempt_member_time ON publish_attempt(principal_id, submitting_at_ms);

CREATE TABLE publish_attempt_item (
    attempt_id INTEGER NOT NULL,
    principal_id INTEGER NOT NULL,
    message_id INTEGER NOT NULL,
    PRIMARY KEY (attempt_id, message_id),
    FOREIGN KEY (attempt_id, principal_id) REFERENCES publish_attempt(id, principal_id),
    FOREIGN KEY (message_id, principal_id) REFERENCES recipient(message_id, principal_id)
);

CREATE INDEX publish_attempt_item_recipient ON publish_attempt_item(principal_id, message_id);
