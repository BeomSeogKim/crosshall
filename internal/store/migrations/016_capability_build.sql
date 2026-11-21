CREATE TABLE capability_build (
    vendor TEXT NOT NULL,
    vendor_version TEXT NOT NULL,
    platform TEXT NOT NULL,
    mode TEXT NOT NULL,
    manifest_rev TEXT NOT NULL,
    cap TEXT NOT NULL CHECK (cap IN ('enroll', 'observe', 'boundary', 'reminder', 'turn_end', 'publish')),
    confirmed INTEGER NOT NULL DEFAULT 0 CHECK (confirmed BETWEEN 0 AND 3),
    stopped_at_ms INTEGER,
    stop_reason TEXT,
    stop_ref INTEGER CHECK (stop_ref > 0),
    lifted_at_ms INTEGER,
    CHECK ((stopped_at_ms IS NULL) = (stop_reason IS NULL)),
    CHECK ((stopped_at_ms IS NULL) = (stop_ref IS NULL)),
    PRIMARY KEY (vendor, vendor_version, platform, mode, manifest_rev, cap)
);
