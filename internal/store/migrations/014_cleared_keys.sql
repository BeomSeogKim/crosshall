-- A key a /clear retired names the conversation that was cleared into its
-- member: that conversation may join as a new member, and is told so.
ALTER TABLE principal_key ADD COLUMN retired_by TEXT CHECK (retired_by IN ('clear'));

-- An owner restore moves a member to a new key; the launches of the old
-- conversation's processes are fenced, so a later /clear there cannot take
-- the member back.
ALTER TABLE adapter_launch ADD COLUMN fenced INTEGER NOT NULL DEFAULT 0 CHECK (fenced IN (0, 1));
