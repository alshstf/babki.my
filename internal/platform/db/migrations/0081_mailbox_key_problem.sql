-- +goose Up
-- A mailbox's app password sealed with a key the program no longer has: noted
-- for the family to state it again.
ALTER TABLE mailboxes DROP CONSTRAINT mailboxes_last_error_check,
    ADD CONSTRAINT mailboxes_last_error_check CHECK (last_error IN ('', 'connect', 'login', 'folder', 'read', 'key'));

-- +goose Down
UPDATE mailboxes SET last_error = 'read' WHERE last_error = 'key';
ALTER TABLE mailboxes DROP CONSTRAINT mailboxes_last_error_check,
    ADD CONSTRAINT mailboxes_last_error_check CHECK (last_error IN ('', 'connect', 'login', 'folder', 'read'));
