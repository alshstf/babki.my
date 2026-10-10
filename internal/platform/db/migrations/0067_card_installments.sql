-- +goose Up
-- Purchases in installments on a credit card (decision Р-33, #458): the
-- months, the fee a month (percent of the sum) and the fee once; the card's
-- default for every purchase — a card of installments («Халва»). The journal
-- row stays as it is; the plan is the card's.
ALTER TABLE credit_cards
    ADD COLUMN installment_months      smallint NOT NULL DEFAULT 0 CHECK (installment_months BETWEEN 0 AND 60),
    ADD COLUMN installment_fee_percent numeric(6,3) NOT NULL DEFAULT 0 CHECK (installment_fee_percent >= 0 AND installment_fee_percent < 100),
    ADD COLUMN installment_fee_minor   bigint NOT NULL DEFAULT 0 CHECK (installment_fee_minor >= 0);

CREATE TABLE card_installments (
    operation_id        uuid PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
    account_id          uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    space_id            uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    months              smallint NOT NULL CHECK (months BETWEEN 1 AND 60),
    monthly_fee_percent numeric(6,3) NOT NULL DEFAULT 0 CHECK (monthly_fee_percent >= 0 AND monthly_fee_percent < 100),
    fee_minor           bigint NOT NULL DEFAULT 0 CHECK (fee_minor >= 0),
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX card_installments_account ON card_installments (account_id);

-- +goose Down
DROP TABLE card_installments;
ALTER TABLE credit_cards
    DROP COLUMN installment_months,
    DROP COLUMN installment_fee_percent,
    DROP COLUMN installment_fee_minor;
