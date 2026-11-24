CREATE UNIQUE INDEX binding_live_principal ON binding(principal_id) WHERE state = 'live';
CREATE UNIQUE INDEX binding_principal_id ON binding(principal_id, id);

CREATE TABLE member_selection (
    principal_id INTEGER PRIMARY KEY REFERENCES principal(id),
    binding_id INTEGER NOT NULL,
    generation INTEGER NOT NULL CHECK (generation > 0),
    selected_at_ms INTEGER NOT NULL,
    FOREIGN KEY (principal_id, binding_id) REFERENCES binding(principal_id, id)
);

INSERT INTO member_selection (principal_id, binding_id, generation, selected_at_ms)
SELECT principal_id, id, 1, joined_at_ms FROM binding;

CREATE TRIGGER member_selection_after_insert AFTER INSERT ON binding
WHEN NEW.state = 'live' OR NOT EXISTS (SELECT 1 FROM member_selection WHERE principal_id = NEW.principal_id)
BEGIN
    INSERT INTO member_selection (principal_id, binding_id, generation, selected_at_ms)
    VALUES (NEW.principal_id, NEW.id, 1, NEW.joined_at_ms)
    ON CONFLICT (principal_id) DO UPDATE SET binding_id = excluded.binding_id, generation = generation + 1,
        selected_at_ms = excluded.selected_at_ms
    WHERE member_selection.binding_id <> excluded.binding_id;
END;

CREATE TRIGGER member_selection_after_rejoin AFTER UPDATE OF state ON binding
WHEN OLD.state = 'departed' AND NEW.state = 'live'
BEGIN
    INSERT INTO member_selection (principal_id, binding_id, generation, selected_at_ms)
    VALUES (NEW.principal_id, NEW.id, 1, NEW.joined_at_ms)
    ON CONFLICT (principal_id) DO UPDATE SET binding_id = excluded.binding_id, generation = generation + 1,
        selected_at_ms = excluded.selected_at_ms
    WHERE member_selection.binding_id <> excluded.binding_id;
END;
