-- +goose Up
-- How a deposit's interest is paid, as chosen at opening, where its variant
-- allows more than one way (at the end, monthly onto a card, added to the
-- sum): the year's tax needs it to know when the interest comes. Null: the
-- variant's only way, or not said.
ALTER TABLE account_programmes ADD COLUMN payout text
    CHECK (payout IS NULL OR payout IN ('end', 'monthly', 'capitalize', 'daily', 'quarterly'));

-- +goose Down
ALTER TABLE account_programmes DROP COLUMN payout;
