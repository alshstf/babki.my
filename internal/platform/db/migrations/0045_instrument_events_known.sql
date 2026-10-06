-- +goose Up
-- Redomiciliations this program knows from the exchange's own notices (decision
-- Р-19): a depositary receipt replaced by the Russian company's share in the
-- trading system, one for one, on a stated day. Written from a list in the code,
-- rewritten on every run, like the exchange's and the feed's rows.
ALTER TABLE instrument_events DROP CONSTRAINT IF EXISTS instrument_events_source_check;
ALTER TABLE instrument_events ADD CONSTRAINT instrument_events_source_check
    CHECK (source IN ('moex_iss', 'yahoo', 'known', 'manual'));

-- +goose Down
DELETE FROM instrument_events WHERE source = 'known';
ALTER TABLE instrument_events DROP CONSTRAINT IF EXISTS instrument_events_source_check;
ALTER TABLE instrument_events ADD CONSTRAINT instrument_events_source_check
    CHECK (source IN ('moex_iss', 'yahoo', 'manual'));
