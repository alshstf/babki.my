-- +goose Up
-- The merchant category code of a card purchase (decision Р-58): four
-- digits, as the bank's statement gives them or the family writes in; what
-- a bank's cashback and a deposit's bonus for spending count by.
ALTER TABLE operations ADD COLUMN mcc text CHECK (mcc IS NULL OR mcc ~ '^[0-9]{4}$');

-- +goose Down
ALTER TABLE operations DROP COLUMN mcc;
