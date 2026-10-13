-- +goose Up
-- The bank or broker that holds the account, from the program's catalog
-- (decision Р-47), when the family picked it; null finds it by the account's
-- «bank or broker» name.
ALTER TABLE accounts ADD COLUMN intermediary text;

-- +goose Down
ALTER TABLE accounts DROP COLUMN intermediary;
