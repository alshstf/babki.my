-- +goose Up
-- A card's bank loyalty programme (decision Р-58): the programme and the
-- variant (the card or the package) of the catalog's. One per account.
CREATE TABLE account_loyalty (
    account_id   uuid PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    space_id     uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    programme_id text NOT NULL,
    variant_id   text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX account_loyalty_space ON account_loyalty (space_id);

-- The categories the family picked for a period, from its first day, and
-- the percent the bank offered on each — a statement does not say them.
CREATE TABLE account_loyalty_picks (
    account_id  uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    space_id    uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    period_from date NOT NULL,
    category_id text NOT NULL,
    percent     numeric(5,2) NOT NULL CHECK (percent > 0 AND percent <= 100),
    PRIMARY KEY (account_id, period_from, category_id)
);
CREATE INDEX account_loyalty_picks_space ON account_loyalty_picks (space_id);

-- +goose Down
DROP TABLE account_loyalty_picks;
DROP TABLE account_loyalty;
