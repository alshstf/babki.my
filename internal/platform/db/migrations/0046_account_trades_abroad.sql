-- +goose Up
-- Whether the account's broker trades on foreign exchanges (Freedom Finance
-- Kazakhstan, Interactive Brokers): there a foreign share sells at its home
-- exchange's price, which then counts as its market price (decision Р-20). On a
-- Russian broker's account the same price is only a reference: what the
-- depository froze in 2022 does not sell at it.
ALTER TABLE accounts ADD COLUMN trades_abroad boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE accounts DROP COLUMN trades_abroad;
