-- +goose Up
CREATE TABLE operations (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    space_id          UUID NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    -- WARNING BEFORE ANYONE ADDS A HARD DELETE FOR AN ACCOUNT (#19). This
    -- cascade is safe only because no such delete exists: the API archives an
    -- account (account.Store.Archive, reached by DELETE /accounts/{id}) and
    -- never removes the row, so the cascade fires only when the whole space
    -- goes, and a space takes both ends of every transfer with it.
    --
    -- A hard delete of ONE account would not. A transfer is a pair of rows in
    -- two different accounts sharing a transfer_group_id, and cascading away
    -- the account on one side leaves the other side's leg standing: shares
    -- arriving from nowhere, or leaving for nowhere, with a cost basis that no
    -- longer has a counterpart. The journal is this program's source of truth
    -- and every position, valuation and profit is recomputed from it, so that
    -- is not an orphaned row in a reporting table — it is a portfolio that
    -- quietly stops adding up.
    --
    -- Whoever adds the delete owes it either a switch to RESTRICT here or an
    -- explicit removal of both legs of every affected pair first (the same
    -- thing operation.Store.Delete already does for a single transfer).
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
