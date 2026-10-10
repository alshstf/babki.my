-- +goose Up
-- The family's budget (decision Р-25, А): a spending category's limit a month
-- from a month on, until a later one of the category; rollover — its
-- «копилка» carries what is left unspent into the next month. An amount of 0
-- without rollover takes the limit off from that month. The spending itself
-- is the journal's, read through the money report.
CREATE TABLE budget_limits (
    space_id     uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    category_id  uuid NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
    from_month   date NOT NULL CHECK (extract(day FROM from_month) = 1),
    amount_minor bigint NOT NULL CHECK (amount_minor >= 0),
    rollover     boolean NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (space_id, category_id, from_month)
);

-- +goose Down
DROP TABLE budget_limits;
