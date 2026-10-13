-- +goose Up
-- A bank's unallocated metal account (ОМС) kept beside bars and coins:
-- grams on the account, valued and taxed as a piece (physical metals, plan
-- step 5).
ALTER TABLE valuables_items DROP CONSTRAINT valuables_items_kind_check;
ALTER TABLE valuables_items ADD CONSTRAINT valuables_items_kind_check CHECK (kind IN ('bar', 'coin', 'account'));

-- +goose Down
DELETE FROM valuables_items WHERE kind = 'account';
ALTER TABLE valuables_items DROP CONSTRAINT valuables_items_kind_check;
ALTER TABLE valuables_items ADD CONSTRAINT valuables_items_kind_check CHECK (kind IN ('bar', 'coin'));
