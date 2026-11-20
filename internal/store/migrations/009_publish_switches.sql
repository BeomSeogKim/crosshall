ALTER TABLE binding ADD COLUMN publish_owner INTEGER CHECK (publish_owner IN (0, 1));

ALTER TABLE binding ADD COLUMN publish_resumed_at_ms INTEGER NOT NULL DEFAULT 0;

ALTER TABLE adapter_policy ADD COLUMN publish INTEGER CHECK (publish IN (0, 1));

CREATE TABLE hall_publish (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    changed_at_ms INTEGER NOT NULL
);

ALTER TABLE capability_evidence RENAME TO capability_evidence_v8;

CREATE TABLE capability_evidence (
    vendor TEXT NOT NULL,
    vendor_version TEXT NOT NULL,
    platform TEXT NOT NULL,
    mode TEXT NOT NULL,
    manifest_rev TEXT NOT NULL,
    cap TEXT NOT NULL CHECK (cap IN ('enroll', 'observe', 'boundary', 'reminder', 'turn_end', 'publish')),
    level TEXT NOT NULL CHECK (level IN ('tested', 'candidate', 'unavailable')),
    result_ref TEXT NOT NULL,
    checked_at_ms INTEGER NOT NULL,
    PRIMARY KEY (vendor, vendor_version, platform, mode, manifest_rev, cap)
);

INSERT INTO capability_evidence (vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms)
SELECT vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms
FROM capability_evidence_v8;

DROP TABLE capability_evidence_v8;
