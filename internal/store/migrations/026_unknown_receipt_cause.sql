-- Which condition left a notice's receipt unknown: an open typed-ahead
-- placeholder, a last turn with no Stop by then, activity after the last
-- Stop, or facts of the launch but no turn. Receipts observed before this
-- migration have none.
ALTER TABLE publish_attempt ADD COLUMN unknown_cause TEXT CHECK (unknown_cause IS NULL OR (observation = 'unknown'
    AND unknown_cause IN ('open_placeholder', 'unfinished_turn', 'activity_since_stop', 'no_turn_but_facts')));
