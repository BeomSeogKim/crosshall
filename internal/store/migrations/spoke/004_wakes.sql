-- The store belongs to one hall and device. The attempt ID stays
-- the same on replay and reconnect.
CREATE TABLE spoke_wake_attempt (
    attempt_id INTEGER PRIMARY KEY CHECK (attempt_id > 0),
    wake TEXT CHECK (json_valid(wake)),
    state TEXT NOT NULL CHECK (state IN ('staged', 'dormant', 'claimed', 'closed', 'finished')),
    submission_seq INTEGER NOT NULL DEFAULT 0 CHECK (submission_seq >= 0),
    outcome TEXT CHECK (outcome IN ('not_submitted', 'submitted', 'uncertain')),
    recorded INTEGER NOT NULL DEFAULT 0 CHECK (recorded IN (0, 1)),
    CHECK (wake IS NOT NULL OR state = 'closed'),
    CHECK (submission_seq > 0 OR state <> 'claimed'),
    CHECK (submission_seq > 0 OR outcome NOT IN ('submitted', 'uncertain')),
    CHECK ((state = 'finished') = (outcome IS NOT NULL))
);

CREATE INDEX spoke_wake_result ON spoke_wake_attempt(attempt_id)
    WHERE state = 'finished' AND recorded = 0;
