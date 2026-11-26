CREATE TABLE spoke_native_route (
    vendor TEXT NOT NULL,
    key TEXT NOT NULL,
    boot TEXT NOT NULL,
    leaf_generation TEXT NOT NULL,
    binding_id INTEGER NOT NULL,
    membership_gen INTEGER NOT NULL,
    route_epoch INTEGER NOT NULL,
    launch TEXT NOT NULL,
    policy_epoch INTEGER NOT NULL,
    capture_seq INTEGER NOT NULL,
    evidence TEXT NOT NULL CHECK (json_valid(evidence)),
    PRIMARY KEY (vendor, key)
);

CREATE TABLE spoke_native_lane (
    target TEXT PRIMARY KEY,
    lane TEXT NOT NULL UNIQUE
);
