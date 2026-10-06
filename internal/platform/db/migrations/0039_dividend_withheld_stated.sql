-- +goose Up
-- The tax withheld abroad from a dividend as a person stated it, from the
-- broker's statement (decision Р-14). It takes the place of the estimate.
-- Keyed by the payment — account, paper, day — rather than by a journal row,
-- so it survives an import rewriting the row.
CREATE TABLE dividend_withheld_stated (
    space_id      UUID NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    account_id    UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    instrument_id UUID NOT NULL REFERENCES instruments(id) ON DELETE CASCADE,
    paid_on       DATE NOT NULL,
    tax_minor     BIGINT NOT NULL CHECK (tax_minor >= 0 AND tax_minor <= 1000000000000000),
    stated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, instrument_id, paid_on)
);

-- +goose Down
DROP TABLE dividend_withheld_stated;
