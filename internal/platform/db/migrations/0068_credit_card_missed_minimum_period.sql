-- +goose Up
-- A minimum missed takes the grace off the purchases of the period it was due
-- in only, those its next statement shows (Т-Банк, #461).
ALTER TABLE credit_cards ADD COLUMN missed_minimum_period boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE credit_cards DROP COLUMN missed_minimum_period;
