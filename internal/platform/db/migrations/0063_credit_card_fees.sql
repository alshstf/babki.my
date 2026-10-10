-- +goose Up
-- What a card's tariff charges besides interest (decision Р-30, #450), to
-- tell it before it is charged: the monthly fee, the cash a period takes
-- free and the fee past it, the fee for a transfer off the card, and the
-- penalty a day on a payment missed. Nothing is written to the journal by
-- them; the bank's charges come there as they happen.
ALTER TABLE credit_cards
    ADD COLUMN monthly_fee_minor        bigint NOT NULL DEFAULT 0 CHECK (monthly_fee_minor >= 0),
    ADD COLUMN cash_free_minor          bigint NOT NULL DEFAULT 0 CHECK (cash_free_minor >= 0),
    ADD COLUMN cash_fee_percent         numeric(6,3) NOT NULL DEFAULT 0 CHECK (cash_fee_percent >= 0 AND cash_fee_percent < 100),
    ADD COLUMN cash_fee_fixed_minor     bigint NOT NULL DEFAULT 0 CHECK (cash_fee_fixed_minor >= 0),
    ADD COLUMN transfer_fee_percent     numeric(6,3) NOT NULL DEFAULT 0 CHECK (transfer_fee_percent >= 0 AND transfer_fee_percent < 100),
    ADD COLUMN transfer_fee_fixed_minor bigint NOT NULL DEFAULT 0 CHECK (transfer_fee_fixed_minor >= 0),
    ADD COLUMN penalty_daily_percent    numeric(6,4) NOT NULL DEFAULT 0 CHECK (penalty_daily_percent >= 0 AND penalty_daily_percent < 10);

-- +goose Down
ALTER TABLE credit_cards
    DROP COLUMN monthly_fee_minor,
    DROP COLUMN cash_free_minor,
    DROP COLUMN cash_fee_percent,
    DROP COLUMN cash_fee_fixed_minor,
    DROP COLUMN transfer_fee_percent,
    DROP COLUMN transfer_fee_fixed_minor,
    DROP COLUMN penalty_daily_percent;
