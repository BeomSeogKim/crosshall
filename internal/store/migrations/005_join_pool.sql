-- The root tokens a channel hands out, one at each principal's first join.
-- A channel that exists before this migration hands out none.
ALTER TABLE channel ADD COLUMN join_pool INTEGER NOT NULL DEFAULT 0 CHECK (join_pool >= 0);
