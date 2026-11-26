-- Hooks on a linked device keep their publish order (#400, follow-up of
-- #421): hook_sent_seq is the highest publish sequence of the leaf's
-- current generation released to the sender; a frame at or below it is
-- late (a later drain pass, or a packet processed again) and is not queued,
-- since the home would take it for a rewound leaf. Each outbox row names
-- its leaf generation, so a replaced leaf's rows do not count.
ALTER TABLE spoke_leaf ADD COLUMN hook_sent_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE spoke_hook_outbox ADD COLUMN generation TEXT NOT NULL DEFAULT '';
CREATE INDEX spoke_hook_outbox_leaf ON spoke_hook_outbox (vendor, key, generation, leaf_seq);
