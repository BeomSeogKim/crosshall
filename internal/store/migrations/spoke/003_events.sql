-- What the home's event stream told this device (#400, PR 6): the
-- stream's cursor, each member's scope and the notices the home prepared
-- for its sessions.

-- The one cursor: the device boot it was acknowledged under, the last
-- event applied and acknowledged, and whether the cache may be served.
-- eligible is 0 from the moment a snapshot is needed until one applies.
CREATE TABLE spoke_event_cursor (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    boot TEXT NOT NULL,
    acked_seq INTEGER NOT NULL,
    eligible INTEGER NOT NULL CHECK (eligible IN (0, 1)),
    -- 1 once the home sent a snapshot below acked_seq: it was restored to
    -- an older store, and the device refuses until it links again.
    rolled_back INTEGER NOT NULL DEFAULT 0 CHECK (rolled_back IN (0, 1))
);

-- A member's scope as the home last sent it.
CREATE TABLE spoke_member_scope (
    vendor TEXT NOT NULL,
    key TEXT NOT NULL,
    leaf_generation TEXT NOT NULL,
    binding_id INTEGER NOT NULL,
    membership_gen INTEGER NOT NULL,
    route_epoch INTEGER NOT NULL,
    PRIMARY KEY (vendor, key)
);

-- A prepared offer: its frame as the home sent it, its lease (epoch and
-- monotonic deadline within boot), and whether a written acknowledgement
-- the home has not recorded yet suppresses it.
CREATE TABLE spoke_offer (
    vendor TEXT NOT NULL,
    key TEXT NOT NULL,
    offer_id INTEGER NOT NULL,
    body TEXT NOT NULL,
    lease_epoch INTEGER NOT NULL,
    deadline_mono INTEGER NOT NULL,
    boot TEXT NOT NULL,
    suppressed INTEGER NOT NULL DEFAULT 0 CHECK (suppressed IN (0, 1)),
    PRIMARY KEY (vendor, key, offer_id)
);
