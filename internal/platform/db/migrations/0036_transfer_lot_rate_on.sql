-- +goose Up
-- The day whose official rate prices a moved parcel's cost in another currency:
-- the settlement day of the purchase behind it, when known (decision Р-3).
-- Null to take acquired_on, as every parcel moved before this column did.
ALTER TABLE operation_transfer_lots ADD COLUMN rate_on DATE;

-- +goose Down
ALTER TABLE operation_transfer_lots DROP COLUMN rate_on;
