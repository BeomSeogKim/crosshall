-- A key a /clear retired names the conversation that was cleared into its
-- member: that conversation may join as a new member, and is told so.
ALTER TABLE principal_key ADD COLUMN retired_by TEXT CHECK (retired_by IN ('clear'));
