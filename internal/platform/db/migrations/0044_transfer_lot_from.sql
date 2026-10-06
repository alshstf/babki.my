-- +goose Up
-- The lot a piece of a breakdown was taken from (plan 2.1): the operation that
-- brought it — an imported one by source and record, a hand entry by id — and
-- its piece number. A transfer, a conversion and a spin-off then name the lots
-- they take instead of matching them by acquisition day. Null on pieces written
-- before lots had numbers.
ALTER TABLE operation_transfer_lots
    ADD COLUMN from_origin TEXT CHECK (from_origin <> ''),
    ADD COLUMN from_seq    INT  CHECK (from_seq >= 0),
    ADD CONSTRAINT operation_transfer_lots_from_pair CHECK ((from_origin IS NULL) = (from_seq IS NULL));

-- +goose Down
ALTER TABLE operation_transfer_lots
    DROP CONSTRAINT IF EXISTS operation_transfer_lots_from_pair,
    DROP COLUMN IF EXISTS from_seq,
    DROP COLUMN IF EXISTS from_origin;
