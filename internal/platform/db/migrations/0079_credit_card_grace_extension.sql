-- +goose Up
-- A running grace extended for a fee (#473, Альфа's «Автопродление периода
-- без %»): the days it goes on to from its start (0: none), the fee in
-- percent of the purchases' debt a month beyond its own days, and whether
-- the next extension is free (the first one is).
ALTER TABLE credit_cards
    ADD COLUMN grace_extend_days    integer NOT NULL DEFAULT 0,
    ADD COLUMN grace_extend_percent numeric NOT NULL DEFAULT 0,
    ADD COLUMN grace_extend_free    boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE credit_cards
    DROP COLUMN grace_extend_days,
    DROP COLUMN grace_extend_percent,
    DROP COLUMN grace_extend_free;
