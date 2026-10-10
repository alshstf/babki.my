-- +goose Up
-- An everyday account — a card, a current account, a deposit, a credit card,
-- a loan, cash — kept by its operations rather than by balance marks
-- (household stage 1): the family total counts its journal, reconciled with
-- the bank's balance as a broker's account is. Off until the family turns it
-- on, so an account kept by its balance does not change. Means nothing on a
-- brokerage account, which is kept by its journal unless valued_by_balance.
ALTER TABLE accounts ADD COLUMN kept_by_operations boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE accounts DROP COLUMN kept_by_operations;
