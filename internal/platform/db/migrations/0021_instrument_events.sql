-- +goose Up
-- A SPLIT IS A FACT ABOUT THE PAPER, NOT ABOUT AN ACCOUNT, and this table is
-- where such facts live.
--
-- Amazon splitting twenty for one happened to every holder of Amazon at once,
-- on one day, whichever broker they hold it at and whichever household they
-- belong to. Recording it once per account would be recording one event many
-- times and inviting the copies to disagree; recording it here, keyed by the
-- ISIN, records it once. What each account then does about it is derived — the
-- journal rows this produces are materialized from these rows and carry
-- source 'registry' (see internal/corporateaction).
--
-- HENCE NO space_id, and that is the same scope the instrument catalog itself
-- has (migration 0004): both describe securities rather than anybody's holdings
-- of them. It is also why this table is not reachable through a space-scoped
-- read: every space in the instance sees the same registry.
--
-- WHY THE BROKER DOES NOT SIMPLY TELL US. Checked against the T-Invest API
-- documentation on 2026-08-22: its operation enum has 71 values and not one of
-- them is a split, a conversion, a spin-off or any other corporate action —
-- their own FAQ says a split changes the quantity in the portfolio and no
-- identifier, with no operation and no method to detect it. So a program that
-- imports operations and nothing else will always show the pre-split quantity
-- and be wrong by exactly the ratio, silently. This registry is the answer to
-- that, and its rows come from the exchange (a daily job over MOEX ISS) or from
-- a person with a source link in their hand.
CREATE TABLE instrument_events (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- What happened. 'split' rewrites quantities of one paper; 'conversion'
    -- turns one paper into another (a depositary receipt into the share it
    -- represented); 'spin_off' leaves the original standing and hands out a
    -- second paper beside it (blocked assets moved into a fund of their own).
    -- Only 'split' is materialized into journals today — the engine types the
    -- other two need are a separate change — but all three are storable now, so
    -- that the facts can be recorded before the arithmetic exists for them.
    kind         text NOT NULL CHECK (kind IN ('split', 'conversion', 'spin_off')),

    -- The paper this happened to, by ISIN — the identity the catalog itself
    -- moved to (migration 0020). NOT an instrument id: a fact about a security
    -- outlives any one catalog row, and an event may well be recorded for a
    -- paper this instance has never catalogued (the exchange job stores every
    -- split it is told about, whether or not anybody here holds the paper).
    isin         text NOT NULL CHECK (isin <> ''),

    -- THE FIRST DAY THE PAPER TRADES IN THE NEW QUANTITY at the venue where it
    -- is held — not the record date, and not the day the decision was taken.
    -- The materialization applies the event at the START of this day: whatever
    -- was held at the close of the day before is multiplied, and a trade dated
    -- this day is already in the new quantity.
    --
    -- Verified against three events on MOEX ISS on 2026-08-22, because the
    -- exchange's own splits table is not consistent about which day of the
    -- suspension window it names: FXUS is dated 2021-10-06 there, the last day
    -- of a two-day halt whose first post-split trade was 2021-10-07 at exactly
    -- one hundredth of the price; NVDA-RM is dated 2021-07-21, again the last
    -- halted day, first new-denomination trade 2021-07-22; T is dated
    -- 2026-04-17, which is the first day trading resumed. All three lie inside
    -- a window with no trades in it, which is why applying at the start of the
    -- stored day gives the right answer for all three whichever end of the
    -- window the source happened to name.
    effective_on date NOT NULL,

    -- The ratio as two whole numbers, the shape the exchange publishes
    -- ("before" and "after"): one unit becomes ratio_to/ratio_from units. A
    -- reverse split is the same fields the other way round (5000 -> 1, as VTBR
    -- did in 2024). Kept as the pair rather than as a computed decimal so that
    -- 1:3 is stored as 1 and 3 and not as 0.3333333333.
    ratio_from   bigint NOT NULL CHECK (ratio_from > 0),
    ratio_to     bigint NOT NULL CHECK (ratio_to > 0),

    -- The paper that comes OUT of a conversion or a spin-off, by ISIN. Null for
    -- a split, which produces no new paper.
    result_isin  text NULL,

    -- What fraction of the original's cost basis moves to the paper a spin-off
    -- produces (НК РФ ст. 277 п. 7: the value of the assets allocated over the
    -- fund's net asset value before the allocation). Null for the other two
    -- kinds: a split moves no basis anywhere and a conversion moves all of it.
    --
    -- Strictly between 0 and 1 by construction: at 0 nothing was allocated and
    -- there is no event, at 1 the original was emptied and that is a conversion
    -- rather than a spin-off.
    basis_share  numeric(20,10) NULL,

    -- Where the fact came from and the evidence for it. 'moex_iss' rows are
    -- written by the daily job and are not a person's to edit or delete;
    -- 'manual' rows carry a link to the disclosure, the exchange notice or the
    -- management company's own announcement, and the API requires it — a
    -- corporate action nobody can point at is a number somebody remembered.
    source       text NOT NULL CHECK (source IN ('moex_iss', 'manual')),
    source_ref   text NOT NULL DEFAULT '',

    -- The exchange's own code for the paper, kept only on rows the exchange
    -- job wrote: that job learns the ISIN by asking ISS what a secid is, and
    -- storing the answer means the next run of the same row costs no second
    -- lookup. It is NOT an identifier anything matches on — the ISIN is.
    moex_secid   text NOT NULL DEFAULT '',

    note         text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),

    -- Who recorded it, for a manual row. Null for the exchange job's rows,
    -- which nobody recorded, and left null rather than removed when a user is
    -- deleted: the event still happened.
    created_by   uuid NULL REFERENCES users(id) ON DELETE SET NULL,

    -- One event of one kind per paper per day. Two rows saying a paper split
    -- twice on one day would each be materialized, and the position would be
    -- multiplied twice — so the constraint that stops the exchange job and a
    -- person recording the same split from producing two rows is the same one
    -- that stops the arithmetic being wrong.
    CONSTRAINT instrument_events_uniq UNIQUE (isin, kind, effective_on),

    -- A conversion and a spin-off name what they produce; a split does not.
    CONSTRAINT instrument_events_result CHECK (
        (kind IN ('conversion', 'spin_off')) = (result_isin IS NOT NULL)
        AND (result_isin IS NULL OR result_isin <> '')
    ),

    -- Only a spin-off divides a cost basis, and it must say how.
    CONSTRAINT instrument_events_basis CHECK (
        (kind = 'spin_off') = (basis_share IS NOT NULL)
        AND (basis_share IS NULL OR (basis_share > 0 AND basis_share < 1))
    )
);

-- The two reads there are. By ISIN: what happened to this paper, which is what
-- the materialization asks for one paper at a time. Whole-table ordered: the
-- registry screen and the sweep, both of which want every row newest first.
CREATE INDEX instrument_events_isin_idx ON instrument_events (isin, effective_on);

-- 'registry' is a journal source of its own, beside the hand entries and the
-- broker imports: rows the registry above materialized into an account. It is a
-- source and not a flag on the row because everything the journal already does
-- per source applies to it unchanged — the write path refuses to let a person
-- delete one (operation.Service.Delete), the import diff reads back exactly the
-- rows its own source wrote, and the T-Invest rebuild goes on owning exactly
-- the rows carrying 'tinvest' and no others.
ALTER TABLE operations DROP CONSTRAINT IF EXISTS operations_source_check;
ALTER TABLE operations ADD CONSTRAINT operations_source_check
    CHECK (source IN ('manual', 'csv', 'tinvest', 'registry'));

-- +goose Down
-- The rows go with the constraint that allowed them: a journal row materialized
-- from the registry has no meaning without the registry, and leaving it behind
-- under a constraint that no longer admits it would fail the downgrade anyway.
DELETE FROM operations WHERE source = 'registry';
ALTER TABLE operations DROP CONSTRAINT IF EXISTS operations_source_check;
ALTER TABLE operations ADD CONSTRAINT operations_source_check
    CHECK (source IN ('manual', 'csv', 'tinvest'));
DROP TABLE instrument_events;
