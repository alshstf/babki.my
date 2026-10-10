-- +goose Up
-- Debt paid ahead of a loan's schedule (#429), and what the bank did with it:
-- shortened the term or lowered the payment. The money moved is in the
-- journal; this says how the schedule changes.
CREATE TABLE loan_prepayments (
    id           uuid PRIMARY KEY,
    account_id   uuid NOT NULL REFERENCES loans(account_id) ON DELETE CASCADE,
    space_id     uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    paid_on      date NOT NULL,
    amount_minor bigint NOT NULL CHECK (amount_minor > 0),
    mode         text NOT NULL CHECK (mode IN ('term', 'payment')),
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX loan_prepayments_account ON loan_prepayments (account_id, paid_on);

-- +goose Down
DROP TABLE loan_prepayments;
