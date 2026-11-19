ALTER TABLE binding ADD COLUMN departed_by_owner INTEGER NOT NULL DEFAULT 0 CHECK (departed_by_owner IN (0, 1));
ALTER TABLE binding ADD COLUMN owner_hook_fence_ms INTEGER;

CREATE INDEX observation_principal_sequence ON observation(principal_id, seq);
