-- +goose Up
-- A family's rules for filing rows (household stage 1, decision Р-24): «when
-- the counterparty (or the note) holds this text, the row goes under that
-- category». The first rule that matches wins; a rule goes with its category.
CREATE TABLE category_rules (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    space_id    uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    category_id uuid NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
    field       text NOT NULL CHECK (field IN ('counterparty', 'note', 'any')),
    pattern     text NOT NULL CHECK (char_length(pattern) BETWEEN 1 AND 200),
    position    integer NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX category_rules_space_idx ON category_rules (space_id, position, created_at);

-- +goose Down
DROP TABLE category_rules;
