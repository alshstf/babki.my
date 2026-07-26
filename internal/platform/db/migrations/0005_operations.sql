-- +goose Up
CREATE TABLE operations (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    space_id          UUID NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    account_id        UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    instrument_id     UUID REFERENCES instruments(id) ON DELETE RESTRICT,
    type              TEXT NOT NULL CHECK (type IN ('buy','sell','deposit','withdrawal','dividend','coupon','amortization','fee','tax','transfer_in','transfer_out','split','interest','conversion')),
    occurred_on       DATE NOT NULL,
    settled_on        DATE,
    quantity          NUMERIC(30,10),
    price             NUMERIC(30,10),
    amount_minor      BIGINT NOT NULL,
    currency          TEXT NOT NULL,
    fee_minor         BIGINT NOT NULL DEFAULT 0 CHECK (fee_minor >= 0),
    note              TEXT NOT NULL DEFAULT '',
    transfer_group_id UUID,
    split_ratio       NUMERIC(20,10),
    source            TEXT NOT NULL DEFAULT 'manual' CHECK (source IN ('manual','csv','tinvest')),
    external_id       TEXT,
    raw               JSONB,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX operations_account_date_idx ON operations (account_id, occurred_on, created_at);
CREATE INDEX operations_space_idx ON operations (space_id);
CREATE UNIQUE INDEX operations_dedup_idx ON operations (account_id, source, external_id)
    WHERE external_id IS NOT NULL;

-- +goose Down
DROP TABLE operations;
