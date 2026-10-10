-- +goose Up
-- What a card's grace covers and how long it runs, as more banks state it
-- (#460): cash and transfers in the grace too (Альфа «без % на всё»),
-- purchases paid some statements later, a category its own number (Ozon:
-- 1, and 3 for «до 140 дней»), the grace's last day at its month's end
-- (Альфа, contracts from 10.08.2026).
ALTER TABLE credit_cards
    ADD COLUMN grace_moves        boolean NOT NULL DEFAULT false,
    ADD COLUMN grace_periods      integer NOT NULL DEFAULT 0 CHECK (grace_periods BETWEEN 0 AND 12),
    ADD COLUMN grace_categories   jsonb NOT NULL DEFAULT '[]',
    ADD COLUMN grace_to_month_end boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE credit_cards
    DROP COLUMN grace_moves,
    DROP COLUMN grace_periods,
    DROP COLUMN grace_categories,
    DROP COLUMN grace_to_month_end;
