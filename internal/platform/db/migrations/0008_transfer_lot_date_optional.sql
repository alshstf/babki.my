-- +goose Up
-- A piece of a transfer's FIFO breakdown may now arrive without an acquisition
-- date. Until this migration the column was NOT NULL, which forced every piece
-- to name a day, and the only way to satisfy that for a parcel whose purchase
-- dates are not knowable was to name one anyway — the transfer's own day. That
-- is a guess wearing the clothes of a fact: it reads exactly like a real
-- purchase date, values the money at the fx rate of a day the shares were not
-- bought on, and sorts into the release queue as if it were the truth.
--
-- The absence is now recorded as an absence. A lot arriving from a transfer
-- with no recoverable dates (a basis typed in by hand, or a transfer written
-- down before breakdowns were kept) can be released, split and amortized like
-- any other, but nothing derived from a date may be computed for it, and the
-- code that would has to say so instead of inventing one.
--
-- EXISTING ROWS ARE NOT TOUCHED: every date already in this table was resolved
-- from the source account's own journal and is real. Only the requirement to
-- have one is dropped.
ALTER TABLE operation_transfer_lots ALTER COLUMN acquired_on DROP NOT NULL;

-- +goose Down
-- Restoring the constraint will FAIL, loudly, if any piece without a date has
-- been written in the meantime. That is the intended behaviour: the only ways
-- to make such rows fit a NOT NULL column are to delete journal data or to fill
-- in a date nobody knows, and a migration must do neither silently. Whoever
-- rolls back has to decide, on the record, what happens to those parcels.
ALTER TABLE operation_transfer_lots ALTER COLUMN acquired_on SET NOT NULL;
