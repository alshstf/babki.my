-- +goose Up
-- A CONVERSION OF ONE PAPER INTO ANOTHER had no type, so it had no honest way
-- into the journal at all. A depositary receipt becomes the share it
-- represented; a fund's units are reissued under a new ISIN. The holder pays
-- nothing and receives nothing: the same money, bought on the same days, simply
-- sits under a different name and a different unit count from that day on.
--
-- Written as a transfer it lost the paper (a transfer keeps the instrument and
-- changes the account; this keeps the account and changes the instrument).
-- Written as a sell plus a buy it invented a disposal — a realized result, and
-- a fresh acquisition date for every lot — where the law says there is none:
-- НК РФ ст. 214.1 п. 13 abz. 17 keeps the receipt's own purchase price as the
-- expense behind the shares received, and ст. 219.1 (389-ФЗ of 2023-07-31)
-- counts the holding period from the day the receipt was bought.
--
-- So the pair is its own thing: exchange_out gives up N units of the old paper
-- and the very lots it names, exchange_in creates M units of the new one from
-- those same lots, each keeping its cost and the day it was acquired.
ALTER TABLE operations DROP CONSTRAINT IF EXISTS operations_type_check;
ALTER TABLE operations ADD CONSTRAINT operations_type_check
    CHECK (type IN ('buy','sell','redemption','deposit','withdrawal','dividend','coupon',
                    'amortization','fee','tax','transfer_in','transfer_out',
                    'exchange_out','exchange_in','split',
                    'interest','conversion'));

-- 'registry' joins the writers. A split or a conversion is a fact about the
-- PAPER — true for everyone who held it on the day — so it is recorded once, in
-- the corporate-actions registry, and applied from there to every account that
-- held the paper. The rows it leaves in a journal are a projection of that
-- record, exactly as an importer's rows are a projection of the broker's, and
-- the source column is what already makes them undeletable one at a time (see
-- Service.Delete): removing one would be undone the next time the registry was
-- applied, and "deleted" would have been a lie.
ALTER TABLE operations DROP CONSTRAINT IF EXISTS operations_source_check;
ALTER TABLE operations ADD CONSTRAINT operations_source_check
    CHECK (source IN ('manual','csv','tinvest','registry'));

-- +goose Down
-- Rows of the retired kinds are DELETED rather than relabelled, and this is the
-- one downgrade in this project that removes data on purpose. There is nothing
-- to relabel them as: a conversion is not a sale, not a purchase and not a
-- transfer, and calling it any of those would leave the account holding either
-- an invented realized result or a paper it does not own. Deleting is also
-- recoverable in the way relabelling is not — the registry still holds the
-- corporate action, and applying it again rebuilds these rows exactly.
--
-- The legs go together: half a conversion is a journal that no longer replays.
DELETE FROM operations WHERE transfer_group_id IN (
    SELECT transfer_group_id FROM operations
    WHERE type IN ('exchange_out','exchange_in') AND transfer_group_id IS NOT NULL);
DELETE FROM operations WHERE type IN ('exchange_out','exchange_in');
DELETE FROM operations WHERE source = 'registry';

ALTER TABLE operations DROP CONSTRAINT IF EXISTS operations_source_check;
ALTER TABLE operations ADD CONSTRAINT operations_source_check
    CHECK (source IN ('manual','csv','tinvest'));

ALTER TABLE operations DROP CONSTRAINT IF EXISTS operations_type_check;
ALTER TABLE operations ADD CONSTRAINT operations_type_check
    CHECK (type IN ('buy','sell','redemption','deposit','withdrawal','dividend','coupon',
                    'amortization','fee','tax','transfer_in','transfer_out','split',
                    'interest','conversion'));
