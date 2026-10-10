-- +goose Up
-- The fingerprint of the catalog version's terms a card's terms were last
-- compared with — taken, applied or kept (alshstf/babki.my#472): the catalog
-- refined without a new revision of the bank's (a mechanic the program
-- learnt, a number corrected) is offered once. NULL for a card taken before:
-- it is offered what differs once.
ALTER TABLE credit_cards ADD COLUMN catalog_terms text;

-- +goose Down
ALTER TABLE credit_cards DROP COLUMN catalog_terms;
