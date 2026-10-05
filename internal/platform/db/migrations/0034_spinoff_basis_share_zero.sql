-- +goose Up
-- A spin-off may move no basis at all: the new paper arrives bought for nothing
-- and the whole cost stays on the original, which is how the broker keeps a
-- carve-out (decision Р-16).
ALTER TABLE instrument_events DROP CONSTRAINT IF EXISTS instrument_events_basis;
ALTER TABLE instrument_events ADD CONSTRAINT instrument_events_basis CHECK (
    (kind = 'spin_off') = (basis_share IS NOT NULL)
    AND (basis_share IS NULL OR (basis_share >= 0 AND basis_share < 1))
);

-- +goose Down
ALTER TABLE instrument_events DROP CONSTRAINT IF EXISTS instrument_events_basis;
ALTER TABLE instrument_events ADD CONSTRAINT instrument_events_basis CHECK (
    (kind = 'spin_off') = (basis_share IS NOT NULL)
    AND (basis_share IS NULL OR (basis_share > 0 AND basis_share < 1))
);
