-- A device's admission generation (#400): the handshake hands a link the
-- current value, and a forwarded request is admitted only while it
-- holds. A retention pass that deletes a device's fences raises it in the
-- same transaction, so no link accepted before a fence outlives the
-- fence, whatever the wall clock does.
ALTER TABLE device ADD COLUMN admission_gen INTEGER NOT NULL DEFAULT 1;

-- A linked device's receipt lookup for a request of a session the home
-- does not know yet (#400): the fence makes the request's absence final,
-- so a first join still queued at the home never runs after the device
-- reported its outcome as unknown. Pruned with operation records.
CREATE TABLE link_fence (
    device_id TEXT NOT NULL REFERENCES device(device_id),
    vendor TEXT NOT NULL,
    key TEXT NOT NULL,
    op_key TEXT NOT NULL,
    link_request TEXT NOT NULL,
    at_ms INTEGER NOT NULL,
    PRIMARY KEY (device_id, vendor, key, op_key)
);
