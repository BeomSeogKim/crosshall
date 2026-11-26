CREATE TRIGGER IF NOT EXISTS device_offer_registration_insert AFTER INSERT ON device_key
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key, membership)
    VALUES (NEW.device_id, NEW.vendor, NEW.key, 1)
    ON CONFLICT(device_id, vendor, key) DO UPDATE SET membership = 1;
END;

CREATE TRIGGER IF NOT EXISTS device_offer_registration_update AFTER UPDATE OF leaf_generation, leaf_epoch, credential_gen, state ON device_key
WHEN OLD.leaf_generation <> NEW.leaf_generation OR OLD.leaf_epoch <> NEW.leaf_epoch
  OR OLD.credential_gen <> NEW.credential_gen OR OLD.state <> NEW.state
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key, invalidate_reason, membership)
    VALUES (NEW.device_id, NEW.vendor, NEW.key, 'route', 1)
    ON CONFLICT(device_id, vendor, key) DO UPDATE SET invalidate_reason = 'route', membership = 1;
END;

CREATE TRIGGER IF NOT EXISTS device_offer_credential_change AFTER UPDATE OF credential_gen ON device
WHEN OLD.credential_gen <> NEW.credential_gen
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key, invalidate_reason, membership)
    SELECT device_id, vendor, key, 'route', 1 FROM device_key WHERE device_id = NEW.device_id AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO UPDATE SET invalidate_reason = 'route', membership = 1;
END;

CREATE TRIGGER IF NOT EXISTS device_offer_selection_insert AFTER INSERT ON member_selection
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key, membership)
    SELECT device_id, vendor, key, 1 FROM device_key WHERE principal_id = NEW.principal_id AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO UPDATE SET membership = 1;
END;

CREATE TRIGGER IF NOT EXISTS device_offer_selection_update AFTER UPDATE OF binding_id, generation ON member_selection
WHEN NEW.binding_id <> OLD.binding_id OR NEW.generation <> OLD.generation
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key, invalidate_reason, membership)
    SELECT device_id, vendor, key, 'move', 1 FROM device_key WHERE principal_id = NEW.principal_id AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO UPDATE SET invalidate_reason = 'move', membership = 1;
END;

CREATE TRIGGER IF NOT EXISTS device_offer_binding AFTER UPDATE OF state, dnd, route_epoch ON binding
WHEN (OLD.state <> NEW.state OR OLD.dnd <> NEW.dnd OR OLD.route_epoch <> NEW.route_epoch)
  AND EXISTS (SELECT 1 FROM member_selection WHERE principal_id = NEW.principal_id AND binding_id = NEW.id)
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key, invalidate_reason, membership)
    SELECT device_id, vendor, key, CASE
        WHEN NEW.state <> 'live' THEN 'depart'
        WHEN OLD.state <> NEW.state OR OLD.route_epoch <> NEW.route_epoch THEN 'route'
        ELSE 'dnd' END,
        OLD.state <> NEW.state OR OLD.route_epoch <> NEW.route_epoch
    FROM device_key WHERE principal_id = NEW.principal_id AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO UPDATE SET
        invalidate_reason = CASE WHEN device_offer_dirty.invalidate_reason = 'move' THEN 'move' ELSE excluded.invalidate_reason END,
        membership = MAX(device_offer_dirty.membership, excluded.membership);
END;

CREATE TRIGGER IF NOT EXISTS device_offer_pending_insert AFTER INSERT ON recipient
WHEN NEW.picked_up_at_ms IS NULL
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key)
    SELECT device_id, vendor, key FROM device_key WHERE principal_id = NEW.principal_id AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO NOTHING;
END;

CREATE TRIGGER IF NOT EXISTS device_offer_pickup AFTER UPDATE OF picked_up_at_ms ON recipient
WHEN OLD.picked_up_at_ms IS NOT NEW.picked_up_at_ms
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key)
    SELECT device_id, vendor, key FROM device_key WHERE principal_id = NEW.principal_id AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO NOTHING;
END;

CREATE TRIGGER IF NOT EXISTS device_offer_obligation AFTER UPDATE OF state ON obligation
WHEN OLD.state <> NEW.state
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key, invalidate_reason)
    SELECT device_id, vendor, key, CASE WHEN NEW.state = 'cancelled' THEN 'cancel' ELSE '' END
    FROM device_key WHERE principal_id = NEW.principal_id AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO UPDATE SET invalidate_reason =
        COALESCE(NULLIF(device_offer_dirty.invalidate_reason, ''), excluded.invalidate_reason);
END;

CREATE TRIGGER IF NOT EXISTS device_offer_policy_insert AFTER INSERT ON adapter_policy
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key, invalidate_reason)
    SELECT device_id, vendor, key, 'policy' FROM device_key WHERE vendor = NEW.adapter AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO UPDATE SET invalidate_reason =
        COALESCE(NULLIF(device_offer_dirty.invalidate_reason, ''), 'policy');
END;

CREATE TRIGGER IF NOT EXISTS device_offer_policy_update AFTER UPDATE OF enabled, epoch ON adapter_policy
WHEN OLD.enabled <> NEW.enabled OR OLD.epoch <> NEW.epoch
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key, invalidate_reason)
    SELECT device_id, vendor, key, 'policy' FROM device_key WHERE vendor = NEW.adapter AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO UPDATE SET invalidate_reason =
        COALESCE(NULLIF(device_offer_dirty.invalidate_reason, ''), 'policy');
END;

CREATE TRIGGER IF NOT EXISTS device_offer_written AFTER UPDATE OF written_at_ms ON offer
WHEN NEW.device_credential_gen IS NOT NULL AND OLD.written_at_ms IS NULL AND NEW.written_at_ms IS NOT NULL
BEGIN
    INSERT INTO device_offer_dirty(device_id, vendor, key)
    SELECT device_id, vendor, key FROM device_key WHERE principal_id = NEW.principal_id AND state = 'live'
    ON CONFLICT(device_id, vendor, key) DO NOTHING;
END;
