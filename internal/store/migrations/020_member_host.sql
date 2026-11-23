-- The agent process that hosts a member's commands, as its last command
-- named it. A live member with no open launch departs when this process is
-- proven gone. One row per principal; several members may share a process.
CREATE TABLE member_host (
    principal_id INTEGER PRIMARY KEY REFERENCES principal(id),
    pid INTEGER NOT NULL CHECK (pid > 0),
    start_identity INTEGER NOT NULL CHECK (start_identity > 0),
    pid_namespace TEXT NOT NULL,
    seen_at_ms INTEGER NOT NULL
);
