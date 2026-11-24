CREATE TABLE observation_with_idle (
    seq INTEGER PRIMARY KEY,
    principal_id INTEGER NOT NULL REFERENCES principal(id),
    kind TEXT NOT NULL CHECK (kind IN ('command', 'prompt_submitted', 'turn_finishing',
        'approval_requested', 'session_ended', 'idle_prompt')),
    origin TEXT NOT NULL CHECK (origin IN ('owner', 'crosshall_wake', 'unknown')),
    source TEXT NOT NULL,
    observed_at_ms INTEGER NOT NULL,
    fresh_until_ms INTEGER,
    launch_ref TEXT,
    event_ref TEXT,
    turn_ref TEXT,
    hook_at_ms INTEGER,
    profile TEXT
);

INSERT INTO observation_with_idle (seq, principal_id, kind, origin, source, observed_at_ms,
    fresh_until_ms, launch_ref, event_ref, turn_ref, hook_at_ms, profile)
SELECT seq, principal_id, kind, origin, source, observed_at_ms,
    fresh_until_ms, launch_ref, event_ref, turn_ref, hook_at_ms, profile
FROM observation;

DROP TABLE observation;
ALTER TABLE observation_with_idle RENAME TO observation;

CREATE INDEX observation_last_command ON observation(principal_id, observed_at_ms) WHERE kind = 'command';
CREATE UNIQUE INDEX observation_event ON observation(principal_id, launch_ref, event_ref)
    WHERE event_ref IS NOT NULL;
CREATE INDEX observation_principal_sequence ON observation(principal_id, seq);
