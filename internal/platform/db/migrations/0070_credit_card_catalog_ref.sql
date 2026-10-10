-- +goose Up
-- The catalog's version a card's terms were taken from (decision Р-32): the
-- product, the version by its first contract day, and the tariff's revision
-- then — to offer a newer revision when the catalog has one.
ALTER TABLE credit_cards
    ADD COLUMN catalog_product        text,
    ADD COLUMN catalog_contracts_from text,
    ADD COLUMN catalog_revision       text,
    ADD CONSTRAINT credit_cards_catalog_ref CHECK ((catalog_product IS NULL) = (catalog_revision IS NULL));

-- +goose Down
ALTER TABLE credit_cards
    DROP CONSTRAINT credit_cards_catalog_ref,
    DROP COLUMN catalog_product,
    DROP COLUMN catalog_contracts_from,
    DROP COLUMN catalog_revision;
