UPDATE message SET thread_reset_after_message_id = (SELECT COALESCE(MAX(id), 0) FROM message)
WHERE kind = 'ask';
UPDATE principal SET reset_after_message_id = (SELECT COALESCE(MAX(id), 0) FROM message),
    any_level_ms = 0, guard_told = 0;
DELETE FROM thread_position;
UPDATE exchange SET episode_open = 0, episode_count = 0;
