-- +goose Up
-- The minimum due by a day of the month (ВТБ: by the 20th), and rounded up
-- to a multiple (ВТБ, Т-Банк: 100 ₽) — #459.
ALTER TABLE credit_cards
    ADD COLUMN pay_day smallint NOT NULL DEFAULT 0 CHECK (pay_day BETWEEN 0 AND 31),
    ADD COLUMN min_round_up_minor bigint NOT NULL DEFAULT 0 CHECK (min_round_up_minor >= 0);

-- +goose Down
ALTER TABLE credit_cards DROP COLUMN pay_day, DROP COLUMN min_round_up_minor;
