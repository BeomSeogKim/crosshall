-- The owner's switch for automatic joining: a session that starts in a Git
-- repository with no channel gets the repository's own channel. No row
-- means off.
CREATE TABLE hall_auto_join (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    changed_at_ms INTEGER NOT NULL
);

-- A default the owner set is theirs; one that came with a channel crosshall
-- created for a repository is automatic and never replaces the owner's.
ALTER TABLE repo_default ADD COLUMN automatic INTEGER NOT NULL DEFAULT 0 CHECK (automatic IN (0, 1));
