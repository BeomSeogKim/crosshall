ALTER TABLE adapter_launch ADD COLUMN process_pid INTEGER CHECK (process_pid > 0);
ALTER TABLE adapter_launch ADD COLUMN process_start_identity INTEGER CHECK (
    (process_pid IS NULL AND process_start_identity IS NULL) OR
    (process_pid IS NOT NULL AND process_start_identity IS NOT NULL AND process_start_identity > 0)
);
ALTER TABLE adapter_launch ADD COLUMN ended_by_process INTEGER NOT NULL DEFAULT 0 CHECK (ended_by_process IN (0, 1));
ALTER TABLE adapter_launch ADD COLUMN pid_namespace TEXT NOT NULL DEFAULT '';

UPDATE adapter_launch AS l
SET process_pid = p.process_pid, process_start_identity = p.process_start_ms
FROM publish_route r
JOIN binding b ON b.id = r.binding_id AND b.principal_id = r.principal_id AND b.route_epoch = r.binding_epoch
JOIN publish_route_proof p ON p.binding_id = r.binding_id AND p.generation = r.generation
WHERE l.principal_id = r.principal_id AND l.launch_ref = r.launch_ref
  AND l.verified = 1 AND l.fenced = 0 AND l.ended_hook_at_ms IS NULL
  AND r.adapter = 'claude' AND r.state = 'active' AND r.launch_verified = 1
  AND (l.launch_ref = 'pid-' || p.process_pid OR l.launch_ref GLOB 'pid-' || p.process_pid || '-*');

CREATE INDEX adapter_launch_open_verified ON adapter_launch (verified)
WHERE verified = 1 AND fenced = 0 AND ended_hook_at_ms IS NULL;
