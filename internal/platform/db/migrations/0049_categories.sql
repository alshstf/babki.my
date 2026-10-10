-- +goose Up
-- A family's categories of money going out and coming in (household stage 1,
-- decision Р-24): a tree two levels deep, each category a spending or an
-- earning one. A space gets the default set the first time it asks for its
-- categories; category_defaults remembers that, so a family that removed them
-- does not get them back.
CREATE TABLE categories (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    space_id   uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    parent_id  uuid REFERENCES categories(id) ON DELETE RESTRICT,
    kind       text NOT NULL CHECK (kind IN ('expense', 'income')),
    name       text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 60),
    archived   boolean NOT NULL DEFAULT false,
    position   integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
-- One name per level of a kind: "Подарки" may be spent and received.
CREATE UNIQUE INDEX categories_name_idx ON categories
    (space_id, kind, COALESCE(parent_id, '00000000-0000-0000-0000-000000000000'::uuid), lower(name));
CREATE INDEX categories_space_idx ON categories (space_id);

CREATE TABLE category_defaults (
    space_id  uuid PRIMARY KEY REFERENCES spaces(id) ON DELETE CASCADE,
    seeded_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE category_defaults;
DROP TABLE categories;
