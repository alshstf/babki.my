-- +goose Up
-- A card whose deadlines on a day off move to the next working day (#452).
ALTER TABLE credit_cards ADD COLUMN shift_to_workday boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE credit_cards DROP COLUMN shift_to_workday;
