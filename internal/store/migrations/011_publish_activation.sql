-- When a proven route first became available, the same verdict status
-- shows. Its watermark is set at that moment, so work stored while the
-- route could not publish (off, capped, held) is never offered. NULL: the
-- transport proved the listener, the route has not been available since.
ALTER TABLE publish_route_proof ADD COLUMN activated_at_ms INTEGER;

-- Earlier proofs set the watermark when they were recorded.
UPDATE publish_route_proof SET activated_at_ms = 0;
