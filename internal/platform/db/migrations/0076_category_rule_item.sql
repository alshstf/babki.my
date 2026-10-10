-- +goose Up
-- A rule may look in a receipt's item names (decision Р-36): «порошок» files
-- that line of a supermarket's receipt under household chemicals, and the
-- row is split across the categories its items fall in.
ALTER TABLE category_rules DROP CONSTRAINT category_rules_field_check;
ALTER TABLE category_rules ADD CONSTRAINT category_rules_field_check
    CHECK (field IN ('counterparty', 'note', 'any', 'item'));

-- +goose Down
DELETE FROM category_rules WHERE field = 'item';
ALTER TABLE category_rules DROP CONSTRAINT category_rules_field_check;
ALTER TABLE category_rules ADD CONSTRAINT category_rules_field_check
    CHECK (field IN ('counterparty', 'note', 'any'));
