-- +goose Up
-- The family's choice for a brokerage account kept by its operations: count it
-- in the total by its balance instead of by its journal, while the journal's
-- history is incomplete (the owner's ruling on Р-2, 2026-10-02). Every account
-- starts on its journal; on any other kind of account the flag means nothing.
ALTER TABLE accounts ADD COLUMN valued_by_balance boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE accounts DROP COLUMN valued_by_balance;
