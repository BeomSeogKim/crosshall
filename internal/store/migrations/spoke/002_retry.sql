-- The key a retry sent a fenced command again under (#400), written
-- before the resend leaves: a repeated retry of the same command
-- resolves that request instead of sending another.
ALTER TABLE link_outbox ADD COLUMN replaced_by TEXT;
