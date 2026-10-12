-- +goose Up
-- An account's tax (decisions Р-41, Р-42): a foreign broker withholds no
-- Russian tax — its client pays by return; a shared account's income is still
-- one person's for tax, null meaning its personal owner, else the space's.
ALTER TABLE accounts ADD COLUMN foreign_broker boolean NOT NULL DEFAULT false;
ALTER TABLE accounts ADD COLUMN tax_owner_user_id uuid REFERENCES users(id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE accounts DROP COLUMN tax_owner_user_id;
ALTER TABLE accounts DROP COLUMN foreign_broker;
