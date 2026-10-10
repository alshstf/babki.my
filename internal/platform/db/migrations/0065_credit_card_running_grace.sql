-- +goose Up
-- A grace that runs from the first purchase (#457): one for every purchase
-- while the card is in debt, grace_days long, from the purchase's day, the
-- day after, or the 1st of its month (ВТБ «110 дней»); a new one only once
-- the debt is repaid in full.
ALTER TABLE credit_cards DROP CONSTRAINT credit_cards_grace_kind_check;
ALTER TABLE credit_cards ADD CONSTRAINT credit_cards_grace_kind_check
    CHECK (grace_kind IN ('statement', 'long', 'windows', 'running'));
ALTER TABLE credit_cards ADD COLUMN grace_run_from text NOT NULL DEFAULT 'purchase'
    CHECK (grace_run_from IN ('purchase', 'next_day', 'month_start'));

-- +goose Down
-- A running grace becomes a long one of as many days.
UPDATE credit_cards SET grace_kind = 'long' WHERE grace_kind = 'running';
ALTER TABLE credit_cards DROP COLUMN grace_run_from;
ALTER TABLE credit_cards DROP CONSTRAINT credit_cards_grace_kind_check;
ALTER TABLE credit_cards ADD CONSTRAINT credit_cards_grace_kind_check
    CHECK (grace_kind IN ('statement', 'long', 'windows'));
