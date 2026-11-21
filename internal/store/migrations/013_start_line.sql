-- A hook-joined member is told who it is in one line once it has company;
-- the member rules follow with its first command (hint_acked).
ALTER TABLE binding ADD COLUMN start_line_seen INTEGER NOT NULL DEFAULT 0 CHECK (start_line_seen IN (0, 1));

-- Before this version a hook-joined member got its rules from the hook's
-- welcome, and its first command only pointed at them. A binding whose
-- welcome was still pending but whose first command was acknowledged has
-- never been given the rules: its next command prints them.
UPDATE binding SET hint_acked = 0 WHERE roster_baseline_seq IS NOT NULL AND hint_acked = 1;

-- The switch for automatic joining kept no row for an "off" that changed
-- nothing. With no row, every recorded switch command was such an off: keep
-- it as the owner's choice, so that a later install does not turn joining on.
INSERT INTO hall_auto_join (id, enabled, changed_at_ms)
SELECT 1, 0, (SELECT MAX(at_ms) FROM op WHERE verb = 'owner channel auto')
WHERE EXISTS (SELECT 1 FROM op WHERE verb = 'owner channel auto')
  AND NOT EXISTS (SELECT 1 FROM hall_auto_join);
