ALTER TABLE publish_suspension ADD COLUMN recovery_hint INTEGER NOT NULL DEFAULT 0 CHECK (recovery_hint IN (0, 1, 2));
