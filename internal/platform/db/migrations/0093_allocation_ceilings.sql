-- +goose Up
-- Ceilings by country of risk and by sector (decision Р-46): «no more than N %
-- of the papers», each on its own, not adding up to 100.
ALTER TABLE allocation_targets DROP CONSTRAINT allocation_targets_dimension_check;
ALTER TABLE allocation_targets ADD CONSTRAINT allocation_targets_dimension_check
    CHECK (dimension IN ('kind', 'currency', 'country', 'sector'));

-- +goose Down
DELETE FROM allocation_targets WHERE dimension IN ('country', 'sector');
ALTER TABLE allocation_targets DROP CONSTRAINT allocation_targets_dimension_check;
ALTER TABLE allocation_targets ADD CONSTRAINT allocation_targets_dimension_check
    CHECK (dimension IN ('kind', 'currency'));
