-- The owner's turn-end switch for an adapter: 1 on, 0 off, NULL when the
-- build's evidence decides, as before the switch existed.
ALTER TABLE adapter_policy ADD COLUMN turn_end INTEGER CHECK (turn_end IN (0, 1));
