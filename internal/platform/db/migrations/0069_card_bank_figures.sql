-- +goose Up
-- What the bank itself says is due on a card (decision Р-32): the payment
-- that keeps the grace and the minimum, each with its day, as read from the
-- bank's app or statement on stated_on. The latest only: until their day
-- they stand over the card's own reckoning.
CREATE TABLE card_bank_figures (
    account_id    uuid PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    space_id      uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    stated_on     date NOT NULL,
    grace_minor   bigint CHECK (grace_minor >= 0),
    grace_on      date,
    minimum_minor bigint CHECK (minimum_minor >= 0),
    minimum_on    date,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CHECK ((grace_minor IS NULL) = (grace_on IS NULL)),
    CHECK ((minimum_minor IS NULL) = (minimum_on IS NULL))
);

-- +goose Down
DROP TABLE card_bank_figures;
