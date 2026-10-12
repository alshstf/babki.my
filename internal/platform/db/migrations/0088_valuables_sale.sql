-- +goose Up
-- A piece sold (physical metals, plan step 4): the day, what it sold for in
-- roubles, and the journal's row of the money, when it was written to an
-- account.
ALTER TABLE valuables_items ADD COLUMN sold_on date;
ALTER TABLE valuables_items ADD COLUMN sold_minor bigint CHECK (sold_minor >= 0);
ALTER TABLE valuables_items ADD COLUMN sale_operation_id uuid REFERENCES operations(id) ON DELETE SET NULL;
ALTER TABLE valuables_items ADD CONSTRAINT valuables_items_sold_check CHECK ((sold_on IS NULL) = (sold_minor IS NULL));

-- +goose Down
ALTER TABLE valuables_items DROP CONSTRAINT valuables_items_sold_check;
ALTER TABLE valuables_items DROP COLUMN sale_operation_id;
ALTER TABLE valuables_items DROP COLUMN sold_minor;
ALTER TABLE valuables_items DROP COLUMN sold_on;
