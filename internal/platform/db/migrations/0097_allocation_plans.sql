-- +goose Up
-- Saved what-if plans (decision Р-48, step 3): the operations as typed, under
-- a name of the family's, with the day they were saved; valued again at
-- today's prices each time they are opened. Nothing of them is in the journal.
CREATE TABLE allocation_plans (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    space_id   uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    name       text NOT NULL CHECK (name <> ''),
    operations jsonb NOT NULL,
    saved_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (space_id, name)
);

-- +goose Down
DROP TABLE allocation_plans;
