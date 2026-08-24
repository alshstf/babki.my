-- +goose Up
-- A SPIN-OFF had no type either, and it is not the conversion this journal
-- learned in 0021. A conversion retires the old paper; a spin-off leaves it
-- standing and hands out a second one beside it, carrying only a SHARE of the
-- money that was paid. Т-Капитал carving the blocked assets out of TECH, TSPX
-- and TUSD into TECH2, TSPX2 and TUSD2 on 2023-12-22 is the owner's own case,
-- and the broker sent no operation for any of it.
--
-- НК РФ ст. 214.1 п. 13 abz. 8 sends the arithmetic to ст. 277 п. 7: the units
-- of the additional fund are worth the part of the original units' cost that
-- the carved-out assets were of the fund's net assets before the carve-out, and
-- the original units' cost goes down by exactly that. No income and no expense
-- arises on the day, so writing it as a sale plus a purchase — the only shapes
-- this journal had — would have invented a realized result and re-dated every
-- parcel to the day of the carve-out.
ALTER TABLE operations DROP CONSTRAINT IF EXISTS operations_type_check;
ALTER TABLE operations ADD CONSTRAINT operations_type_check
    CHECK (type IN ('buy','sell','redemption','deposit','withdrawal','dividend','coupon',
                    'amortization','fee','tax','transfer_in','transfer_out',
                    'exchange_out','exchange_in','spinoff_out','spinoff_in','split',
                    'interest','conversion'));

-- A PIECE MAY NOW NAME A PARCEL OF NO UNITS, which the breakdown table refused
-- from the day it was created. Until now every piece was a quantity that MOVED,
-- and moving nothing is not an event. A spin-off's departing leg moves no units
-- at all: its pieces name the parcels whose money went, and each carries that
-- parcel's own count as its identity, so that a later replay can see whether
-- the position it was struck against is still the position in the journal (see
-- portfolio.Position.applySpinoffOut). A parcel whose shares a reverse split
-- rounded away holds real money and a count of zero, and it has to be nameable.
--
-- Nothing else loosens: the count still cannot be negative, and the pieces of a
-- transfer and of a conversion are still held to summing to the quantity of the
-- row that carries them (portfolio.CheckTransferLots), which no zero-quantity
-- piece can satisfy on its own.
ALTER TABLE operation_transfer_lots DROP CONSTRAINT IF EXISTS operation_transfer_lots_quantity_check;
ALTER TABLE operation_transfer_lots ADD CONSTRAINT operation_transfer_lots_quantity_check
    CHECK (quantity >= 0);

-- +goose Down
-- The legs go together, exactly as 0021's do: half a spin-off is a journal that
-- no longer replays. The registry still holds the corporate action, so applying
-- it again rebuilds these rows.
DELETE FROM operations WHERE transfer_group_id IN (
    SELECT transfer_group_id FROM operations
    WHERE type IN ('spinoff_out','spinoff_in') AND transfer_group_id IS NOT NULL);
DELETE FROM operations WHERE type IN ('spinoff_out','spinoff_in');

-- Any remaining zero-quantity piece belongs to a row that has just been
-- deleted, so this is a restoration rather than a loss.
DELETE FROM operation_transfer_lots WHERE quantity = 0;

ALTER TABLE operation_transfer_lots DROP CONSTRAINT IF EXISTS operation_transfer_lots_quantity_check;
ALTER TABLE operation_transfer_lots ADD CONSTRAINT operation_transfer_lots_quantity_check
    CHECK (quantity > 0);

ALTER TABLE operations DROP CONSTRAINT IF EXISTS operations_type_check;
ALTER TABLE operations ADD CONSTRAINT operations_type_check
    CHECK (type IN ('buy','sell','redemption','deposit','withdrawal','dividend','coupon',
                    'amortization','fee','tax','transfer_in','transfer_out',
                    'exchange_out','exchange_in','split',
                    'interest','conversion'));
