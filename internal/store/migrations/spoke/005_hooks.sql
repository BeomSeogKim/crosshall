-- Hooks on a linked device (#400, 6s-b): the hall's adapter policies as
-- the home's event stream carries them, and the hook facts and written
-- acknowledgements waiting for the home's Recorded.
CREATE TABLE spoke_policy (
    adapter TEXT PRIMARY KEY,
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    reminder INTEGER NOT NULL CHECK (reminder IN (0, 1)),
    epoch INTEGER NOT NULL CHECK (epoch >= 0)
);

-- One row per Fact or Written, by the ID the home records it under; the
-- body is the frame without its ID. A row goes once the home answered it.
-- They go out per leaf in the order of their leaf publish sequence.
CREATE TABLE spoke_hook_outbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    vendor TEXT NOT NULL,
    key TEXT NOT NULL,
    leaf_seq INTEGER NOT NULL CHECK (leaf_seq > 0),
    kind TEXT NOT NULL CHECK (kind IN ('fact', 'written')),
    body TEXT NOT NULL,
    at_ms INTEGER NOT NULL
);
