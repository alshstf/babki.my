-- +goose Up
-- One load of a table into an account: what was loaded, how it was read and
-- what came of it. The operations it wrote are listed beside it, so the whole
-- load can be taken back at once.
CREATE TABLE table_imports (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    space_id       UUID NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    account_id     UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    file_name      TEXT NOT NULL DEFAULT '',
    mapping        JSONB NOT NULL,
    rows_written   INT NOT NULL CHECK (rows_written >= 0),
    rows_duplicate INT NOT NULL CHECK (rows_duplicate >= 0),
    rows_unparsed  INT NOT NULL CHECK (rows_unparsed >= 0),
    rows_refused   INT NOT NULL CHECK (rows_refused >= 0),
    created_by     UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    rolled_back_at TIMESTAMPTZ
);
CREATE INDEX table_imports_account_idx ON table_imports (account_id, created_at DESC);

-- An operation leaves this list when it leaves the journal.
CREATE TABLE table_import_operations (
    import_id    UUID NOT NULL REFERENCES table_imports(id) ON DELETE CASCADE,
    operation_id UUID NOT NULL REFERENCES operations(id) ON DELETE CASCADE,
    PRIMARY KEY (import_id, operation_id)
);
CREATE INDEX table_import_operations_operation_idx ON table_import_operations (operation_id);

-- +goose Down
DROP TABLE table_import_operations;
DROP TABLE table_imports;
