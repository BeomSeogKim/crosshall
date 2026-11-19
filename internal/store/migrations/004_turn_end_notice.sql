ALTER TABLE capability_evidence RENAME TO capability_evidence_v3;

CREATE TABLE capability_evidence (
    vendor TEXT NOT NULL,
    vendor_version TEXT NOT NULL,
    platform TEXT NOT NULL,
    mode TEXT NOT NULL,
    manifest_rev TEXT NOT NULL,
    cap TEXT NOT NULL CHECK (cap IN ('enroll', 'observe', 'boundary', 'reminder', 'turn_end')),
    level TEXT NOT NULL CHECK (level IN ('tested', 'candidate', 'unavailable')),
    result_ref TEXT NOT NULL,
    checked_at_ms INTEGER NOT NULL,
    PRIMARY KEY (vendor, vendor_version, platform, mode, manifest_rev, cap)
);

INSERT INTO capability_evidence (vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms)
SELECT vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms
FROM capability_evidence_v3;

DROP TABLE capability_evidence_v3;

CREATE TABLE turn_end_notice (
    id INTEGER PRIMARY KEY,
    principal_id INTEGER NOT NULL,
    adapter TEXT NOT NULL,
    launch_ref TEXT NOT NULL,
    turn_ref TEXT NOT NULL,
    at_ms INTEGER NOT NULL,
    UNIQUE (principal_id, launch_ref, turn_ref),
    UNIQUE (id, principal_id),
    FOREIGN KEY (principal_id, launch_ref, turn_ref) REFERENCES turn(principal_id, launch_ref, turn_ref)
);

CREATE INDEX turn_end_notice_adapter_time ON turn_end_notice(adapter, at_ms);

CREATE TABLE turn_end_notice_item (
    notice_id INTEGER NOT NULL,
    principal_id INTEGER NOT NULL,
    message_id INTEGER NOT NULL,
    PRIMARY KEY (principal_id, message_id),
    FOREIGN KEY (notice_id, principal_id) REFERENCES turn_end_notice(id, principal_id),
    FOREIGN KEY (message_id, principal_id) REFERENCES recipient(message_id, principal_id)
);
