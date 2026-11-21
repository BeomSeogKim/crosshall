-- A hook-joined member is told who it is in one line once it has company;
-- the member rules follow with its first command (hint_acked).
ALTER TABLE binding ADD COLUMN start_line_seen INTEGER NOT NULL DEFAULT 0 CHECK (start_line_seen IN (0, 1));
