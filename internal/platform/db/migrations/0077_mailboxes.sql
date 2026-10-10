-- +goose Up
-- A mailbox for receipts (decision Р-35, А): a box kept for receipts alone,
-- the family's mail forwarding the shops' and OFD letters there; the program
-- reads that box only, over IMAP with TLS, by an app password sealed with the
-- encryption key. last_uid and uid_validity say which letters were read.
CREATE TABLE mailboxes (
    space_id        uuid PRIMARY KEY REFERENCES spaces(id) ON DELETE CASCADE,
    host            text NOT NULL CHECK (char_length(host) BETWEEN 1 AND 255),
    port            integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    username        text NOT NULL CHECK (char_length(username) BETWEEN 1 AND 320),
    password_sealed bytea NOT NULL,
    folder          text NOT NULL DEFAULT 'INBOX' CHECK (char_length(folder) BETWEEN 1 AND 255),
    uid_validity    bigint NOT NULL DEFAULT 0,
    last_uid        bigint NOT NULL DEFAULT 0,
    checked_at      timestamptz,
    last_error      text NOT NULL DEFAULT '' CHECK (last_error IN ('', 'connect', 'login', 'folder', 'read')),
    last_found      integer NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE mailboxes;
