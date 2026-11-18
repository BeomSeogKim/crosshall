CREATE TABLE channel (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    topic TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'archived')),
    created_at_ms INTEGER NOT NULL
);

CREATE TABLE repo_default (
    repo_root TEXT NOT NULL PRIMARY KEY,
    channel_id INTEGER NOT NULL REFERENCES channel(id)
);

CREATE TABLE principal (
    id INTEGER PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('session', 'owner')),
    vendor TEXT NOT NULL,
    vendor_version TEXT,
    root_tokens INTEGER NOT NULL DEFAULT 0 CHECK (root_tokens >= 0),
    created_at_ms INTEGER NOT NULL,
    ended_at_ms INTEGER
);

CREATE UNIQUE INDEX principal_one_owner ON principal(kind) WHERE kind = 'owner';

CREATE TABLE principal_key (
    vendor TEXT NOT NULL,
    key TEXT NOT NULL,
    principal_id INTEGER NOT NULL REFERENCES principal(id),
    state TEXT NOT NULL DEFAULT 'live' CHECK (state IN ('live', 'retired')),
    created_at_ms INTEGER NOT NULL,
    retired_at_ms INTEGER,
    PRIMARY KEY (vendor, key),
    UNIQUE (vendor, key, principal_id)
);

CREATE TABLE leaf (
    digest TEXT NOT NULL PRIMARY KEY CHECK (length(digest) = 64 AND digest NOT GLOB '*[^a-f0-9]*'),
    vendor TEXT NOT NULL,
    key TEXT NOT NULL,
    principal_id INTEGER,
    generation TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'live' CHECK (state IN ('live', 'fenced')),
    file_identity TEXT NOT NULL,
    created_at_ms INTEGER NOT NULL,
    FOREIGN KEY (vendor, key, principal_id) REFERENCES principal_key(vendor, key, principal_id)
);

CREATE TABLE binding (
    id INTEGER PRIMARY KEY,
    principal_id INTEGER NOT NULL REFERENCES principal(id),
    channel_id INTEGER NOT NULL REFERENCES channel(id),
    alias TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'live' CHECK (state IN ('live', 'departed')),
    joined_at_ms INTEGER NOT NULL,
    departed_at_ms INTEGER,
    focus TEXT NOT NULL DEFAULT '',
    focus_at_ms INTEGER,
    context_exchange_id INTEGER REFERENCES exchange(id),
    roster_ack_seq INTEGER NOT NULL DEFAULT 0 CHECK (roster_ack_seq >= 0),
    hint_acked INTEGER NOT NULL DEFAULT 0 CHECK (hint_acked IN (0, 1)),
    wake_pref INTEGER NOT NULL DEFAULT 1 CHECK (wake_pref IN (0, 1)),
    dnd INTEGER NOT NULL DEFAULT 0 CHECK (dnd IN (0, 1)),
    UNIQUE (principal_id, channel_id)
);

CREATE UNIQUE INDEX binding_live_alias ON binding(channel_id, alias) WHERE state = 'live';

CREATE TABLE message (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    channel_id INTEGER NOT NULL REFERENCES channel(id),
    sender_id INTEGER NOT NULL REFERENCES principal(id),
    kind TEXT NOT NULL CHECK (kind IN ('note', 'ask', 'reply')),
    outcome TEXT CHECK (outcome IN ('answer', 'decline', 'more_time')),
    in_reply_to INTEGER REFERENCES message(id),
    exchange_id INTEGER REFERENCES exchange(id),
    audience_mode TEXT NOT NULL CHECK (audience_mode IN ('explicit', 'all')),
    due_at_ms INTEGER,
    body TEXT NOT NULL,
    created_at_ms INTEGER NOT NULL
);

CREATE INDEX message_channel_sender ON message(channel_id, sender_id, id);

CREATE TABLE recipient (
    message_id INTEGER NOT NULL REFERENCES message(id),
    principal_id INTEGER NOT NULL REFERENCES principal(id),
    alias_at_commit TEXT NOT NULL,
    owes INTEGER NOT NULL CHECK (owes IN (0, 1)),
    picked_up_at_ms INTEGER,
    PRIMARY KEY (message_id, principal_id)
);

CREATE INDEX recipient_unread ON recipient(principal_id, message_id) WHERE picked_up_at_ms IS NULL;

CREATE TABLE obligation (
    message_id INTEGER NOT NULL,
    principal_id INTEGER NOT NULL,
    state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'replied', 'declined', 'cancelled')),
    settled_at_ms INTEGER,
    settled_by_id INTEGER REFERENCES principal(id),
    settlement_message_id INTEGER REFERENCES message(id),
    more_time_at_ms INTEGER,
    reminded_at_ms INTEGER,
    PRIMARY KEY (message_id, principal_id),
    FOREIGN KEY (message_id, principal_id) REFERENCES recipient(message_id, principal_id)
);

CREATE INDEX obligation_open ON obligation(principal_id, message_id) WHERE state = 'open';

CREATE TABLE receipt (
    message_id INTEGER NOT NULL,
    principal_id INTEGER NOT NULL,
    stage TEXT NOT NULL CHECK (stage IN ('stored', 'picked_up', 'settled', 'notified', 'held', 'wake_attempted', 'wake_unconfirmed')),
    seq INTEGER NOT NULL,
    at_ms INTEGER NOT NULL,
    evidence TEXT NOT NULL,
    PRIMARY KEY (message_id, principal_id, stage, seq),
    FOREIGN KEY (message_id, principal_id) REFERENCES recipient(message_id, principal_id)
);

CREATE TABLE exchange (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    channel_id INTEGER NOT NULL REFERENCES channel(id),
    root_message_id INTEGER NOT NULL UNIQUE REFERENCES message(id) DEFERRABLE INITIALLY DEFERRED,
    opened_by_id INTEGER NOT NULL REFERENCES principal(id),
    granted INTEGER NOT NULL CHECK (granted >= 0),
    spent INTEGER NOT NULL DEFAULT 0 CHECK (spent >= 0 AND spent <= granted),
    episode_open INTEGER NOT NULL DEFAULT 0 CHECK (episode_open IN (0, 1)),
    episode_count INTEGER NOT NULL DEFAULT 0 CHECK (episode_count >= 0)
);

CREATE TABLE budget_event (
    seq INTEGER PRIMARY KEY,
    exchange_id INTEGER REFERENCES exchange(id),
    principal_id INTEGER REFERENCES principal(id),
    delta INTEGER NOT NULL,
    reason TEXT NOT NULL CHECK (reason IN ('initial', 'debit', 'grant', 'roots')),
    message_id INTEGER REFERENCES message(id),
    by_id INTEGER NOT NULL REFERENCES principal(id),
    at_ms INTEGER NOT NULL,
    CHECK ((exchange_id IS NULL) <> (principal_id IS NULL))
);

CREATE TABLE op (
    principal_id INTEGER NOT NULL REFERENCES principal(id),
    op_key TEXT NOT NULL,
    scope TEXT NOT NULL,
    verb TEXT NOT NULL,
    payload_hash TEXT NOT NULL,
    result TEXT NOT NULL,
    at_ms INTEGER NOT NULL,
    PRIMARY KEY (principal_id, op_key)
);

CREATE TABLE roster_change (
    channel_id INTEGER NOT NULL REFERENCES channel(id),
    seq INTEGER NOT NULL,
    principal_id INTEGER NOT NULL REFERENCES principal(id),
    field TEXT NOT NULL,
    PRIMARY KEY (channel_id, seq)
);

CREATE TABLE observation (
    seq INTEGER PRIMARY KEY,
    principal_id INTEGER NOT NULL REFERENCES principal(id),
    kind TEXT NOT NULL CHECK (kind IN ('command', 'prompt_submitted', 'turn_finishing', 'approval_requested', 'session_ended')),
    origin TEXT NOT NULL CHECK (origin IN ('owner', 'crosshall_wake', 'unknown')),
    source TEXT NOT NULL,
    observed_at_ms INTEGER NOT NULL,
    fresh_until_ms INTEGER
);

CREATE INDEX observation_last_command ON observation(principal_id, observed_at_ms) WHERE kind = 'command';

CREATE TABLE capability (
    principal_id INTEGER NOT NULL REFERENCES principal(id),
    cap TEXT NOT NULL CHECK (cap IN ('enroll', 'observe', 'boundary', 'wake')),
    level TEXT NOT NULL CHECK (level IN ('tested', 'candidate', 'unavailable')),
    available INTEGER NOT NULL CHECK (available IN (0, 1)),
    vendor_version TEXT NOT NULL,
    platform TEXT NOT NULL,
    mode TEXT NOT NULL,
    checked_at_ms INTEGER NOT NULL,
    PRIMARY KEY (principal_id, cap)
);

CREATE TABLE delivery_attempt (
    seq INTEGER PRIMARY KEY,
    message_set TEXT NOT NULL,
    principal_id INTEGER NOT NULL REFERENCES principal(id),
    route TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('recorded', 'accepted', 'failed_pre_accept', 'unconfirmed')),
    at_ms INTEGER NOT NULL
);

CREATE TABLE result_wait (
    message_id INTEGER PRIMARY KEY REFERENCES message(id),
    asker_id INTEGER NOT NULL REFERENCES principal(id),
    state TEXT NOT NULL CHECK (state IN ('armed', 'consumed')),
    consumed_reason TEXT CHECK (consumed_reason IN ('all_settled', 'deadline')),
    consumed_at_ms INTEGER
);

CREATE TABLE waiter_lease (
    principal_id INTEGER PRIMARY KEY REFERENCES principal(id),
    lease_token TEXT NOT NULL,
    holder TEXT NOT NULL,
    armed_at_ms INTEGER NOT NULL,
    renewed_at_ms INTEGER NOT NULL,
    expires_at_ms INTEGER NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('live', 'expired', 'released')),
    reason TEXT
);

CREATE TABLE event (
    seq INTEGER PRIMARY KEY,
    kind TEXT NOT NULL,
    subject TEXT NOT NULL,
    actor_id INTEGER REFERENCES principal(id),
    detail TEXT NOT NULL,
    at_ms INTEGER NOT NULL
);
