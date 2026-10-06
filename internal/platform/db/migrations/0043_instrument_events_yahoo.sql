-- +goose Up
-- Splits of foreign papers come from the share feed (Yahoo Finance), which the
-- exchange's table does not cover; its rows are rewritten on every run, like
-- the exchange's.
ALTER TABLE instrument_events DROP CONSTRAINT IF EXISTS instrument_events_source_check;
ALTER TABLE instrument_events ADD CONSTRAINT instrument_events_source_check
    CHECK (source IN ('moex_iss', 'yahoo', 'manual'));

-- +goose Down
DELETE FROM instrument_events WHERE source = 'yahoo';
ALTER TABLE instrument_events DROP CONSTRAINT IF EXISTS instrument_events_source_check;
ALTER TABLE instrument_events ADD CONSTRAINT instrument_events_source_check
    CHECK (source IN ('moex_iss', 'manual'));
