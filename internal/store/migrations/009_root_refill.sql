ALTER TABLE channel ADD COLUMN root_refill_per_hour INTEGER NOT NULL DEFAULT 3
    CHECK (root_refill_per_hour BETWEEN 0 AND 3600000);
ALTER TABLE channel ADD COLUMN root_refill_ceiling INTEGER NOT NULL DEFAULT 5
    CHECK (root_refill_ceiling >= 0);

ALTER TABLE binding ADD COLUMN root_refill_per_hour INTEGER
    CHECK (root_refill_per_hour BETWEEN 0 AND 3600000);
ALTER TABLE binding ADD COLUMN root_refill_ceiling INTEGER
    CHECK (root_refill_ceiling >= 0);

ALTER TABLE principal ADD COLUMN root_refill_at_ms INTEGER;
ALTER TABLE principal ADD COLUMN root_refill_fraction INTEGER NOT NULL DEFAULT 0
    CHECK (root_refill_fraction BETWEEN 0 AND 3599999);

UPDATE principal SET root_refill_at_ms = CAST(unixepoch('subsec') * 1000 AS INTEGER)
WHERE kind = 'session';
