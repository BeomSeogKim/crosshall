CREATE TABLE owner_alert (
    event_seq INTEGER PRIMARY KEY REFERENCES event(seq),
    desktop_state TEXT NOT NULL DEFAULT 'pending'
        CHECK (desktop_state IN ('pending', 'sending', 'sent', 'failed', 'unavailable', 'skipped')),
    desktop_at_ms INTEGER,
    shown_at_ms INTEGER
);
