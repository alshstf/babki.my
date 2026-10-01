-- +goose Up
-- The corporate-actions registry finds the accounts that hold a paper by
-- joining operations to instruments on the instrument id, once per paper on
-- every sweep and after every recorded event. Without an index that is a scan
-- of the whole journal each time.
--
-- Partial for the reason 0010 gives: cash-level rows carry no instrument, and
-- nothing looks rows up by the absence of one.
CREATE INDEX operations_instrument_idx ON operations (instrument_id)
    WHERE instrument_id IS NOT NULL;

-- +goose Down
DROP INDEX operations_instrument_idx;
