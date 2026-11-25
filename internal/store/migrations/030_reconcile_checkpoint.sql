-- The last reassignment of a launch's turns: the newest observation it could
-- read and when it ran. A later reassignment refolds only what can have
-- changed since. NULL means unknown: the next one reads every fact (#366).
ALTER TABLE adapter_launch ADD COLUMN reconciled_seq INTEGER;
ALTER TABLE adapter_launch ADD COLUMN reconciled_at_ms INTEGER;

-- The placeholder turns still running, or finished late: a refold from a
-- turn needs none before it (#366).
CREATE INDEX turn_placeholder_finishing ON turn(principal_id, launch_ref, finishing_hook_at_ms)
    WHERE turn_ref >= 'typed-ahead:' AND turn_ref < 'typed-ahead;';
