-- +goose Up
-- The tariff's fees as more banks state them (#462): a fee a year with the
-- statement after the first spending (Т-Банк), transfers free up to a part
-- a period (Т-Банк), cash and transfers free up to an amount in the first
-- days from the contract (ВТБ), the penalty in percent a year (Т-Банк) and
-- from a day of lateness («Халва»).
ALTER TABLE credit_cards
    ADD COLUMN yearly_fee_minor       bigint NOT NULL DEFAULT 0 CHECK (yearly_fee_minor >= 0),
    ADD COLUMN transfer_free_minor    bigint NOT NULL DEFAULT 0 CHECK (transfer_free_minor >= 0),
    ADD COLUMN intro_free_days        integer NOT NULL DEFAULT 0 CHECK (intro_free_days BETWEEN 0 AND 366),
    ADD COLUMN intro_free_minor       bigint NOT NULL DEFAULT 0 CHECK (intro_free_minor >= 0),
    ADD COLUMN penalty_yearly_percent numeric(7,4) NOT NULL DEFAULT 0 CHECK (penalty_yearly_percent >= 0 AND penalty_yearly_percent < 1000),
    ADD COLUMN penalty_from_day       integer NOT NULL DEFAULT 0 CHECK (penalty_from_day BETWEEN 0 AND 90);

-- +goose Down
ALTER TABLE credit_cards
    DROP COLUMN yearly_fee_minor,
    DROP COLUMN transfer_free_minor,
    DROP COLUMN intro_free_days,
    DROP COLUMN intro_free_minor,
    DROP COLUMN penalty_yearly_percent,
    DROP COLUMN penalty_from_day;
