-- +goose Up
-- On an amortization: a bond's outstanding face value per unit just before the
-- repayment, in the operation's currency. With it the repayment retires the
-- cost basis in proportion to the principal it returns (НК РФ ст. 214.1 п. 13,
-- decision Р-4); without it, the old rule. Null on every other row.
ALTER TABLE operations ADD COLUMN face_before_minor BIGINT
    CHECK (face_before_minor IS NULL OR (type = 'amortization' AND face_before_minor > 0));

-- +goose Down
ALTER TABLE operations DROP COLUMN face_before_minor;
