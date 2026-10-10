-- +goose Up
-- A row's category and counterparty (household stage 1, decision Р-24): a
-- spending is a withdrawal with a spending category, an earning a deposit with
-- an earning one. The engine reads neither. A category in use cannot be
-- removed, only archived, so the rows keep it.
ALTER TABLE operations
    ADD COLUMN category_id  uuid REFERENCES categories(id) ON DELETE RESTRICT,
    ADD COLUMN counterparty text NOT NULL DEFAULT '' CHECK (char_length(counterparty) <= 200);
CREATE INDEX operations_category_idx ON operations (space_id, category_id) WHERE category_id IS NOT NULL;

-- +goose Down
DROP INDEX operations_category_idx;
ALTER TABLE operations DROP COLUMN counterparty, DROP COLUMN category_id;
