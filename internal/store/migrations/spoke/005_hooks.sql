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

-- The membership a leaf's stamp file carries and the leaf publish sequence
-- at which it started carrying it: a membership change is cut there, so a
-- hook that captured the new stamp before the home's event arrived is
-- after the cut.
ALTER TABLE spoke_leaf ADD COLUMN stamp_binding INTEGER NOT NULL DEFAULT 0;
ALTER TABLE spoke_leaf ADD COLUMN stamp_gen INTEGER NOT NULL DEFAULT 0;
ALTER TABLE spoke_leaf ADD COLUMN stamp_seq INTEGER NOT NULL DEFAULT 0;

-- The leaf publish sequence of the last cut acknowledged for the member's
-- scope: a repeated change of the same scope never cuts earlier.
ALTER TABLE spoke_member_scope ADD COLUMN cut_seq INTEGER NOT NULL DEFAULT 0;
