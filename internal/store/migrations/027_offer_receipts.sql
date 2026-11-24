ALTER TABLE offer ADD COLUMN receipt_mode TEXT NOT NULL DEFAULT 'legacy'
    CHECK (receipt_mode IN ('historical', 'legacy', 'provisional'));
ALTER TABLE offer ADD COLUMN route_epoch INTEGER CHECK (route_epoch > 0);
ALTER TABLE offer ADD COLUMN nonce TEXT;
ALTER TABLE offer ADD COLUMN written_at_ms INTEGER;

UPDATE offer SET receipt_mode = 'historical';
CREATE INDEX offer_unconfirmed_scope ON offer(principal_id, binding_id, generation, launch_ref, epoch, route_epoch)
    WHERE receipt_mode = 'provisional' AND written_at_ms IS NULL;

-- A clean stamp may have been saved for a line whose output is unknown.
INSERT INTO adapter_dirty(principal_id, notice)
SELECT DISTINCT r.principal_id, 1
FROM offer_item oi
JOIN recipient r ON r.principal_id = oi.principal_id AND r.message_id = oi.message_id
JOIN message m ON m.id = r.message_id
JOIN binding b ON b.principal_id = r.principal_id AND b.channel_id = m.channel_id
WHERE b.state = 'live' AND b.dnd = 0 AND r.picked_up_at_ms IS NULL
    AND m.outcome IS NOT 'more_time'
    AND NOT EXISTS (SELECT 1 FROM obligation o WHERE o.message_id = m.id
        AND o.principal_id = r.principal_id AND o.state <> 'open')
    AND NOT EXISTS (SELECT 1 FROM announcement a WHERE a.message_id = m.id
        AND a.principal_id = r.principal_id AND a.via = 'turn_end')
    AND NOT EXISTS (SELECT 1 FROM publish_attempt_item i JOIN publish_attempt p ON p.id = i.attempt_id
        WHERE i.message_id = m.id AND i.principal_id = r.principal_id
            AND p.observation IN ('own_turn', 'absorbed', 'unknown'))
ON CONFLICT(principal_id) DO UPDATE SET notice = 1;
