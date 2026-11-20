ALTER TABLE binding ADD COLUMN publish_owner INTEGER CHECK (publish_owner IN (0, 1));

-- The order in which transport outcomes were recorded. An attempt
-- reserved before a resume and resolved after it is counted after it.
ALTER TABLE publish_attempt ADD COLUMN outcome_seq INTEGER;

UPDATE publish_attempt SET outcome_seq = id WHERE resolved_at_ms IS NOT NULL;

CREATE INDEX publish_attempt_outcome ON publish_attempt(binding_id, launch_ref, outcome_seq);

-- The owner's last resume, in commit order: suspensions with a higher ID
-- and outcomes with a higher sequence came after it.
ALTER TABLE binding ADD COLUMN publish_resume_suspension INTEGER NOT NULL DEFAULT 0;

ALTER TABLE binding ADD COLUMN publish_resume_outcome INTEGER NOT NULL DEFAULT 0;

ALTER TABLE adapter_policy ADD COLUMN publish INTEGER CHECK (publish IN (0, 1));

CREATE TABLE publish_suspension (
    id INTEGER PRIMARY KEY,
    binding_id INTEGER NOT NULL REFERENCES binding(id),
    launch_ref TEXT NOT NULL,
    reason TEXT NOT NULL CHECK (reason IN ('absorbed', 'unknown', 'not_submitted')),
    notice INTEGER NOT NULL,
    at_ms INTEGER NOT NULL,
    until_ms INTEGER
);

CREATE INDEX publish_suspension_launch ON publish_suspension(binding_id, launch_ref);

-- A store from before this migration held a launch after any absorbed or
-- unknown notice. Each becomes a suspension under the new rules: thirty
-- minutes, or the rest of the launch from the third on that launch.
INSERT INTO publish_suspension (binding_id, launch_ref, reason, notice, at_ms, until_ms)
SELECT binding_id, launch_ref, observation, id, observed_at_ms,
    CASE WHEN ROW_NUMBER() OVER (PARTITION BY binding_id, launch_ref ORDER BY id) >= 3
        THEN NULL ELSE observed_at_ms + 1800000 END
FROM publish_attempt
WHERE observation IN ('absorbed', 'unknown') AND observed_at_ms IS NOT NULL
ORDER BY id;

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
    -- Who recorded the row: an owner import, or the profile list the binary
    -- ships. Imports cannot set it.
    source TEXT NOT NULL DEFAULT 'owner' CHECK (source IN ('owner', 'builtin')),
    PRIMARY KEY (vendor, vendor_version, platform, mode, manifest_rev, cap)
);

INSERT INTO capability_evidence (vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms)
SELECT vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms
FROM capability_evidence_v8;

DROP TABLE capability_evidence_v8;
