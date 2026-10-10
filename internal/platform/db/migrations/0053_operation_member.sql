-- +goose Up
-- Whose a row is (household stage 1, «чья трата»): the member who spent or
-- earned it, when that is not simply the owner of the account — a shared card
-- both partners pay with. Null means the account's owner, or the family for a
-- shared account. A member who leaves takes nothing with them; the rows stay.
ALTER TABLE operations ADD COLUMN member_id uuid REFERENCES users(id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE operations DROP COLUMN member_id;
