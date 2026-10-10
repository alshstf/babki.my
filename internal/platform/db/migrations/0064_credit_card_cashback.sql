-- +goose Up
-- A card's cashback rules, to tell the cashback before it comes (decision
-- Р-31, #451): a percent on every purchase, higher ones in the family's
-- categories (with their subcategories), a cap a month, whether it comes as
-- the bank's points rather than money, and the days after the statement it
-- comes in. The journal keeps the cashback that came.
ALTER TABLE credit_cards
    ADD COLUMN cashback_base_percent numeric(6,3) NOT NULL DEFAULT 0 CHECK (cashback_base_percent >= 0 AND cashback_base_percent < 100),
    ADD COLUMN cashback_categories   jsonb NOT NULL DEFAULT '[]',
    ADD COLUMN cashback_cap_minor    bigint NOT NULL DEFAULT 0 CHECK (cashback_cap_minor >= 0),
    ADD COLUMN cashback_points       boolean NOT NULL DEFAULT false,
    ADD COLUMN cashback_credit_days  smallint NOT NULL DEFAULT 0 CHECK (cashback_credit_days BETWEEN 0 AND 60);

-- +goose Down
ALTER TABLE credit_cards
    DROP COLUMN cashback_base_percent,
    DROP COLUMN cashback_categories,
    DROP COLUMN cashback_cap_minor,
    DROP COLUMN cashback_points,
    DROP COLUMN cashback_credit_days;
