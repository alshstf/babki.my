-- +goose Up
CREATE TABLE accounts (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    space_id      UUID NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    owner_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    name          TEXT NOT NULL,
    type          TEXT NOT NULL CHECK (type IN ('brokerage','checking','savings','deposit','credit_card','loan','cash')),
    currency      TEXT NOT NULL,
    institution   TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','archived')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX accounts_space_idx ON accounts (space_id);

-- Manual balance marks: one per account per date; latest date wins.
CREATE TABLE account_balances (
    account_id   UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    as_of        DATE NOT NULL,
    amount_minor BIGINT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, as_of)
);

-- +goose Down
DROP TABLE account_balances;
DROP TABLE accounts;
