-- +goose Up
-- A spending or an earning split across categories (decision Р-36): a
-- supermarket's receipt holds groceries, household chemicals and beer, and
-- each part counts under its own category in the money report and the
-- budget. The parts' amounts, positive, add up to the row's; the row's own
-- category stays the one it is filed under elsewhere.
CREATE TABLE operation_parts (
    operation_id uuid NOT NULL REFERENCES operations(id) ON DELETE CASCADE,
    space_id     uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    position     integer NOT NULL CHECK (position >= 0),
    category_id  uuid NOT NULL REFERENCES categories(id) ON DELETE RESTRICT,
    amount_minor bigint NOT NULL CHECK (amount_minor > 0),
    PRIMARY KEY (operation_id, position)
);
CREATE INDEX operation_parts_category ON operation_parts (category_id);

-- +goose Down
DROP TABLE operation_parts;
