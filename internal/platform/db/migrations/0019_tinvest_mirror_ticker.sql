-- +goose Up
-- The mirror kept every identifier the broker sends for an instrument except
-- the one that survives the broker forgetting it.
--
-- A fund wound up or a company redomiciled, and the passport answers 404 for
-- ever after — while the owner's history stays full of operations on it. The
-- operation itself is then the only description of the paper left, and for
-- exactly such an instrument the broker puts the ISIN in its TICKER field
-- ("IE00BD3QJN10", "RU000A101X68" — the FinEx funds on the owner's own
-- account). An ISIN is a globally unique security identifier, so matching it
-- against the catalog is proof rather than a guess (see Resolver.resolveOne).
--
-- NOT THE FIGI, which the mirror has held all along and which is the wrong
-- identifier here: the broker re-issues it per listing, so the catalog holds
-- TCS20A101X68 for the very paper whose operations carry TCS33A101X68. Only
-- the ISIN crosses the move.
--
-- The backfill reads the payload the mirror already stored, so an existing
-- installation recovers every ticker without asking the broker again — the
-- property the mirror exists for.
ALTER TABLE tinvest_operations_mirror
    ADD COLUMN ticker TEXT NOT NULL DEFAULT '';

UPDATE tinvest_operations_mirror
   SET ticker = raw ->> 'ticker'
 WHERE raw ? 'ticker'
   AND raw ->> 'ticker' IS NOT NULL;

-- content_key is deliberately NOT rebuilt to include the new column, for the
-- reason 0016 states at length: the key identifies an operation across syncs,
-- and changing its formula would make every row already in the mirror look
-- like one that disappeared and a new one that arrived.

-- +goose Down
ALTER TABLE tinvest_operations_mirror
    DROP COLUMN ticker;
