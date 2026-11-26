-- Whether the device's last event stream negotiated the policy capability:
-- the home appends adapter policy events only for such a device (#400).
ALTER TABLE link_event_stream ADD COLUMN policy_capable INTEGER NOT NULL DEFAULT 0 CHECK (policy_capable IN (0, 1));
