-- +goose Up
-- A credit card's terms (decision Р-26): its limit, the day the bank closes
-- each statement period, the days given to pay after it, how the interest-free
-- period runs — to the statement's payment date, or a long stretch from the
-- start of each period — the monthly minimum payment, the rate charged once
-- the grace is lost, and the rate the family's own money would earn instead,
-- for weighing the card against it. The journal stays the record of what was
-- spent and paid; what is due is worked out from both.
CREATE TABLE credit_cards (
    account_id      uuid PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    space_id        uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    limit_minor     bigint NOT NULL CHECK (limit_minor >= 0),
    statement_day   smallint NOT NULL CHECK (statement_day BETWEEN 1 AND 31),
    payment_days    smallint NOT NULL CHECK (payment_days BETWEEN 0 AND 60),
    grace_kind      text NOT NULL CHECK (grace_kind IN ('statement', 'long')),
    grace_days      smallint NOT NULL CHECK (grace_days BETWEEN 0 AND 1100),
    min_percent     numeric(5,2) NOT NULL CHECK (min_percent >= 0 AND min_percent <= 100),
    min_floor_minor bigint NOT NULL CHECK (min_floor_minor >= 0),
    annual_rate     numeric(9,4) NOT NULL CHECK (annual_rate >= 0 AND annual_rate < 1000),
    own_rate        numeric(9,4) CHECK (own_rate >= 0 AND own_rate < 1000),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE credit_cards;
