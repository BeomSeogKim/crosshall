-- A handoff exchange links to the request its opener still owes an answer to.
ALTER TABLE exchange ADD COLUMN parent_request_id INTEGER REFERENCES message(id);
