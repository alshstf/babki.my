-- +goose Up
-- The categories a card's bank takes for transfers, not purchases (decision
-- Р-29, #449): a top-up of a wallet or a broker, a transfer through a
-- service, a bet — the bank tells them by the shop's code, which the journal
-- does not have. Their spending on this card has no grace: interest from its
-- day. Subcategories go with their parent.
ALTER TABLE credit_cards ADD COLUMN transfer_categories uuid[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE credit_cards DROP COLUMN transfer_categories;
