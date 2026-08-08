-- +goose Up
-- The mirror kept only the broker's `quantity`, which on a partially filled
-- order is the size of the ORDER rather than of the fill. The executed size is
-- a separate field, `quantityDone`, and the projection needs it: without it a
-- purchase of 6644 units enters the journal as 11100 units for the same money
-- (#131).
--
-- The backfill reads the payload the mirror already stored, so an existing
-- installation recovers every executed size without asking the broker again —
-- which is the property the mirror exists for. Rows whose payload has no such
-- field (non-trades, and the one currency trade the broker sends without it)
-- get 0, and 0 is not a size: the projection refuses a trade that has none
-- rather than falling back to the order size (see projectTrade).
ALTER TABLE tinvest_operations_mirror
    ADD COLUMN quantity_done BIGINT NOT NULL DEFAULT 0;

UPDATE tinvest_operations_mirror
   SET quantity_done = (raw ->> 'quantityDone')::BIGINT
 WHERE raw ? 'quantityDone'
   AND (raw ->> 'quantityDone') ~ '^-?[0-9]+$';

-- content_key is deliberately NOT rebuilt to include the new column. The key
-- identifies an operation across syncs, and changing its formula would make
-- every row already in the mirror look like one that disappeared and a new one
-- that arrived — thousands of phantom changes, and a journal rebuilt under new
-- external ids, to record a field the payment already distinguishes.

-- +goose Down
ALTER TABLE tinvest_operations_mirror DROP COLUMN quantity_done;
