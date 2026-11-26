-- A device's credential generation: every re-enrollment under its name
-- raises it, and a link authenticated under an older one is refused (#400).
ALTER TABLE device ADD COLUMN credential_gen INTEGER NOT NULL DEFAULT 1;

-- The device a session member works on; NULL for the home device's own
-- sessions (#400).
ALTER TABLE principal ADD COLUMN device_id TEXT REFERENCES device(device_id);

-- The keys of members on linked devices, scoped by device so two devices
-- cannot collide on a key. leaf_epoch orders the device's leaf
-- registrations under one credential generation; a request from an older
-- registration is refused (#400).
CREATE TABLE device_key (
    device_id TEXT NOT NULL REFERENCES device(device_id),
    vendor TEXT NOT NULL,
    key TEXT NOT NULL,
    principal_id INTEGER NOT NULL REFERENCES principal(id),
    leaf_generation TEXT NOT NULL,
    leaf_epoch INTEGER NOT NULL,
    credential_gen INTEGER NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('live', 'retired')),
    created_at_ms INTEGER NOT NULL,
    PRIMARY KEY (device_id, vendor, key)
);
CREATE INDEX device_key_principal ON device_key(principal_id);

-- For a command forwarded by a linked device: the hash of the forwarded
-- request as it arrived and the membership it was admitted under, so an
-- exact retry after a lost response gets its committed receipt (#400).
ALTER TABLE op ADD COLUMN link_request TEXT;
