CREATE TRIGGER IF NOT EXISTS link_wake_new_boot AFTER UPDATE OF boot ON link_event_cursor
WHEN OLD.boot <> NEW.boot
BEGIN
    UPDATE link_wake_attempt SET released_at_ms = CAST(unixepoch('subsec') * 1000 AS INTEGER)
    WHERE device_id = NEW.device_id AND json_extract(route, '$.boot') <> NEW.boot
        AND state <> 'not_submitted' AND released_at_ms IS NULL;
END;

CREATE TRIGGER IF NOT EXISTS link_wake_new_credential AFTER UPDATE OF credential_gen ON device
WHEN OLD.credential_gen <> NEW.credential_gen
BEGIN
    UPDATE link_wake_attempt SET released_at_ms = NEW.secret_at_ms
    WHERE device_id = NEW.device_id AND credential_gen <> NEW.credential_gen
        AND state <> 'not_submitted' AND released_at_ms IS NULL;
END;

CREATE TRIGGER IF NOT EXISTS link_wake_new_leaf AFTER UPDATE OF leaf_generation ON device_key
WHEN OLD.leaf_generation <> NEW.leaf_generation
BEGIN
    UPDATE link_wake_attempt SET released_at_ms = CAST(unixepoch('subsec') * 1000 AS INTEGER)
    WHERE principal_id = NEW.principal_id AND device_id = NEW.device_id AND vendor = NEW.vendor
        AND json_extract(route, '$.key') = NEW.key
        AND json_extract(route, '$.scope.leaf_generation') <> NEW.leaf_generation
        AND state <> 'not_submitted' AND released_at_ms IS NULL;
END;

CREATE TRIGGER IF NOT EXISTS link_wake_absent_leaf AFTER UPDATE OF absent ON link_event_cut
WHEN OLD.absent = 0 AND NEW.absent = 1
BEGIN
    UPDATE link_wake_attempt SET released_at_ms = CAST(unixepoch('subsec') * 1000 AS INTEGER)
    WHERE device_id = NEW.device_id AND vendor = NEW.vendor AND binding_id = NEW.binding_id
        AND json_extract(route, '$.key') = NEW.key
        AND json_extract(route, '$.boot') = NEW.boot
        AND json_extract(route, '$.scope.leaf_generation') = NEW.leaf_generation
        AND json_extract(route, '$.scope.membership.generation') = NEW.membership_generation
        AND state <> 'not_submitted' AND released_at_ms IS NULL;
END;

CREATE TRIGGER IF NOT EXISTS link_wake_new_selection AFTER UPDATE OF binding_id, generation ON member_selection
WHEN OLD.binding_id <> NEW.binding_id OR OLD.generation <> NEW.generation
BEGIN
    UPDATE link_wake_attempt SET released_at_ms = NEW.selected_at_ms
    WHERE principal_id = NEW.principal_id
        AND (binding_id <> NEW.binding_id OR json_extract(route, '$.scope.membership.generation') <> NEW.generation)
        AND state <> 'not_submitted' AND released_at_ms IS NULL;
END;

CREATE TRIGGER IF NOT EXISTS link_wake_departed AFTER UPDATE OF state ON binding
WHEN OLD.state = 'live' AND NEW.state = 'departed'
BEGIN
    UPDATE link_wake_attempt SET released_at_ms = COALESCE(NEW.departed_at_ms, CAST(unixepoch('subsec') * 1000 AS INTEGER))
    WHERE principal_id = NEW.principal_id AND binding_id = NEW.id
        AND state <> 'not_submitted' AND released_at_ms IS NULL;
END;
