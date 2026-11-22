-- What the hall last told a member about reaching it: NULL nothing, 1 that
-- it brings new work to the session, 0 that it cannot reach it now.
ALTER TABLE binding ADD COLUMN reach_told INTEGER CHECK (reach_told IN (0, 1));
