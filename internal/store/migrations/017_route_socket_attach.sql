-- Whether the session's start saw the vendor's socket, and what the last
-- report of a socket path did to the route (#180).
ALTER TABLE publish_route ADD COLUMN start_socket TEXT CHECK (start_socket IN ('seen', 'missing'));
ALTER TABLE publish_route ADD COLUMN socket_attach TEXT CHECK (socket_attach IN ('start', 'later', 'same', 'ignored'));
ALTER TABLE publish_route ADD COLUMN socket_attach_at_ms INTEGER;

-- A member's requests read its keys by principal.
CREATE INDEX principal_key_principal ON principal_key(principal_id);
