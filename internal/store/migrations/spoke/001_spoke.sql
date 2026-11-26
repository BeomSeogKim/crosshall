-- The store of a device linked to a home (#400): its own sessions'
-- registrations, and the forwarded commands whose answers it is waiting
-- for. One store per hall and device; a link to another hall never sees
-- this one.

-- A session registered on this device: its leaf directory's identity and
-- generation, and an epoch that rises with every new registration of the
-- same session, which the home orders registrations by.
CREATE TABLE spoke_leaf (
    digest TEXT PRIMARY KEY,
    vendor TEXT NOT NULL,
    key TEXT NOT NULL,
    generation TEXT NOT NULL,
    epoch INTEGER NOT NULL,
    file_identity TEXT NOT NULL,
    vendor_version TEXT NOT NULL DEFAULT '',
    created_at_ms INTEGER NOT NULL,
    UNIQUE (vendor, key)
);

-- A keyed member command that may have reached the home, written before
-- it is sent: the exact command bytes and membership, so a lost answer
-- can be looked up receipt-only. state is sent until the answer arrives
-- (done), or unknown when the link ended first.
CREATE TABLE link_outbox (
    id INTEGER PRIMARY KEY,
    vendor TEXT NOT NULL,
    key TEXT NOT NULL,
    op_key TEXT NOT NULL,
    command TEXT NOT NULL,
    args TEXT NOT NULL,
    membership TEXT,
    state TEXT NOT NULL CHECK (state IN ('sent', 'unknown', 'done')),
    result TEXT,
    at_ms INTEGER NOT NULL,
    UNIQUE (vendor, key, op_key)
);
