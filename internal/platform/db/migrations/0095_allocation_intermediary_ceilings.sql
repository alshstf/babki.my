-- +goose Up
-- Ceilings by broker and by bank (decision Р-47): «no more than N %» of what
-- the brokers, or the banks, hold, each on its own.
ALTER TABLE allocation_targets DROP CONSTRAINT allocation_targets_dimension_check;
ALTER TABLE allocation_targets ADD CONSTRAINT allocation_targets_dimension_check
    CHECK (dimension IN ('kind', 'currency', 'country', 'sector', 'broker', 'bank'));

-- +goose Down
DELETE FROM allocation_targets WHERE dimension IN ('broker', 'bank');
ALTER TABLE allocation_targets DROP CONSTRAINT allocation_targets_dimension_check;
ALTER TABLE allocation_targets ADD CONSTRAINT allocation_targets_dimension_check
    CHECK (dimension IN ('kind', 'currency', 'country', 'sector'));
