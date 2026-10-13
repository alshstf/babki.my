-- +goose Up
-- A wage payment's gross and the tax withheld from it as its payslip says
-- (decision Р-41): entered by hand over the program's reckoning from what
-- reached the account.
CREATE TABLE wage_entries (
    operation_id   uuid PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
    space_id       uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    gross_minor    bigint NOT NULL CHECK (gross_minor > 0),
    withheld_minor bigint NOT NULL CHECK (withheld_minor >= 0 AND withheld_minor <= gross_minor)
);
CREATE INDEX wage_entries_space_idx ON wage_entries (space_id);

-- +goose Down
DROP TABLE wage_entries;
