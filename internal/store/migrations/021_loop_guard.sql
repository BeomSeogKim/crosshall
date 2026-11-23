ALTER TABLE message ADD COLUMN level_ms INTEGER NOT NULL DEFAULT 0 CHECK (level_ms >= 0);
ALTER TABLE principal ADD COLUMN any_level_ms INTEGER NOT NULL DEFAULT 0 CHECK (any_level_ms >= 0);
ALTER TABLE principal ADD COLUMN reset_after_message_id INTEGER NOT NULL DEFAULT 0 CHECK (reset_after_message_id >= 0);
ALTER TABLE principal ADD COLUMN round_limit INTEGER NOT NULL DEFAULT 8 CHECK (round_limit >= 0);
ALTER TABLE principal ADD COLUMN guard_told INTEGER NOT NULL DEFAULT 0 CHECK (guard_told >= 0);
ALTER TABLE exchange ADD COLUMN reset_after_message_id INTEGER NOT NULL DEFAULT 0 CHECK (reset_after_message_id >= 0);
ALTER TABLE exchange ADD COLUMN round_limit INTEGER NOT NULL DEFAULT 8 CHECK (round_limit >= 0);

CREATE TABLE thread_position (
    principal_id INTEGER NOT NULL REFERENCES principal(id),
    exchange_id INTEGER NOT NULL REFERENCES exchange(id),
    level_ms INTEGER NOT NULL CHECK (level_ms >= 0),
    PRIMARY KEY (principal_id, exchange_id)
) WITHOUT ROWID;

CREATE INDEX message_ask_burst ON message(sender_id, created_at_ms) WHERE kind = 'ask';
