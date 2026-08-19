-- +goose Up
-- A bond reaching maturity was recorded as a sale, because the journal had no
-- other type for it. The arithmetic was right — the bonds leave, the money
-- arrives, the queue gives up their basis — and the word was wrong: nobody sold
-- anything, the paper ran out.
--
-- The journal already named the PARTIAL repayment separately (amortization), so
-- the full one was the only disposal masquerading as something else.
--
-- No row is rewritten here. Imported redemptions are rebuilt from the mirror on
-- the next sync — which is what the mirror is for — and there is nothing else to
-- convert: a "sell" written by hand is a sale, and this migration has no way to
-- tell one that was really a redemption apart from one that was not. Guessing
-- would relabel real sales.
ALTER TABLE operations DROP CONSTRAINT IF EXISTS operations_type_check;
ALTER TABLE operations ADD CONSTRAINT operations_type_check
    CHECK (type IN ('buy','sell','redemption','deposit','withdrawal','dividend','coupon',
                    'amortization','fee','tax','transfer_in','transfer_out','split',
                    'interest','conversion'));

-- +goose Down
-- Anything recorded as a redemption becomes a sale again rather than blocking
-- the downgrade: that is what it was before, and it is what the arithmetic has
-- always treated it as.
UPDATE operations SET type = 'sell' WHERE type = 'redemption';
ALTER TABLE operations DROP CONSTRAINT IF EXISTS operations_type_check;
ALTER TABLE operations ADD CONSTRAINT operations_type_check
    CHECK (type IN ('buy','sell','deposit','withdrawal','dividend','coupon',
                    'amortization','fee','tax','transfer_in','transfer_out','split',
                    'interest','conversion'));
