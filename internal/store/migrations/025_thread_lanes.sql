-- The server process that held a thread-routed session's thread at its
-- start, and the vendor home it serves, as that start named them. The
-- adapter's transport proves both.
ALTER TABLE publish_route ADD COLUMN host_pid INTEGER CHECK (host_pid > 0);
ALTER TABLE publish_route ADD COLUMN host_start INTEGER CHECK (host_start > 0);
ALTER TABLE publish_route ADD COLUMN vendor_home TEXT CHECK (vendor_home <> '');

-- The queue a notice holds until it is observed or retired: the launch's
-- process for a process-routed adapter, the thread in its home for a
-- thread-routed one, since every thread of one server shares that server's
-- process and each home numbers its own threads.
ALTER TABLE publish_attempt ADD COLUMN lane TEXT;
UPDATE publish_attempt SET lane = 'process:' || process_pid || ':' || process_start_ms;

DROP INDEX publish_attempt_queue;
CREATE UNIQUE INDEX publish_attempt_lane ON publish_attempt(lane)
    WHERE observed_at_ms IS NULL AND retired_at_ms IS NULL AND state <> 'not_submitted';
