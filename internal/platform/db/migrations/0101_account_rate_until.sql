-- +goose Up
-- The last day a rate written in by hand holds (decision Р-59 А): after it the
-- bank's programme the account is tied to gives the rate again, or the next
-- rate written in. Null: until the next one.
ALTER TABLE account_rates ADD COLUMN until_on date,
    ADD CHECK (until_on IS NULL OR until_on >= from_on);

-- +goose Down
ALTER TABLE account_rates DROP COLUMN until_on;
