-- The hall's identity. A linked device checks it before it proves its
-- secret, so ssh access to another hall cannot pass for this one (#400).
CREATE TABLE hall (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    hall_id TEXT NOT NULL
);
INSERT INTO hall (singleton, hall_id) VALUES (1, lower(hex(randomblob(16))));

-- Devices linked to this hall as spokes. The secret never leaves the
-- dispatcher: a link proves it as an HMAC of a fresh nonce (#400).
CREATE TABLE device (
    device_id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    secret BLOB NOT NULL,
    created_at_ms INTEGER NOT NULL,
    secret_at_ms INTEGER NOT NULL
);
