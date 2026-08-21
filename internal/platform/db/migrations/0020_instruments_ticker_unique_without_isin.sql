-- +goose Up
-- A TICKER IS NOT A NAME FOR A SECURITY, and treating it as one refused a
-- perfectly ordinary pair of papers.
--
-- Migration 0011 made the ticker unique among the rows a price is fetched for,
-- and said in as many words what it was giving up: "the same ticker on two
-- exchanges is one instrument here". The owner met that limit on live data. His
-- catalog holds AT&T under "T" (US00206R1023); his broker reports Т-Технологии,
-- also under "T" (RU000A107UL4). They are two companies on two exchanges, and
-- the second could not be catalogued at all — so every operation on it stayed
-- unimported, with the resolver refusing (correctly) to attach them to AT&T.
--
-- THE IDENTITY OF A SECURITY IS ITS ISIN, which is globally unique by
-- construction. So uniqueness moves to where identity actually lives: a row
-- that carries an ISIN is identified by it and may share a ticker with anything,
-- while rows with NO ISIN keep the old rule among themselves — there the ticker
-- is all there is, and two hand-entered papers reading "SBER" are still the
-- duplicate 0011 was written to stop.
--
-- The new index is strictly narrower than the one it replaces, so no existing
-- database can fail this upgrade: everything the old rule allowed, the new one
-- allows. 0011's guard is not repeated for that reason.
--
-- WHAT 0011 WAS PROTECTING is not given up with it. Its argument was about the
-- quotes job, which maps ticker -> instrument and would silently price one of
-- two rows and never the other (#26). That map now matches on the ticker AND
-- the currency the provider itself reports for the price, which is exactly what
-- separates these two papers: MOEX answers "T" in rubles, which is Т-Технологии,
-- and never AT&T's dollars (see marketdata.refreshQuotesWorker). Two rows left
-- ambiguous even after that — same ticker, same currency — are priced neither
-- way and say so, rather than one of them losing silently.
-- AND UNIQUENESS ARRIVES WHERE IT WAS MISSING. Dropping the ticker rule without
-- this would drop a protection nobody meant to give up: two connections
-- resolving one paper at the same moment used to collide on the ticker, and the
-- loser retried and found the winner's row (see Resolver.findOrCreate). Without
-- a rule they would both succeed and the catalog would grow two rows for one
-- security — which is the duplicate 0011 was written to stop, arriving by
-- another door. The ISIN is what that race should have been colliding on all
-- along.
--
-- Databases holding duplicates are stopped with a message that says what to do,
-- the way 0011 does and for the same reason: letting CREATE UNIQUE INDEX fail
-- on its own names a Postgres object rather than the problem, and merging rows
-- unattended would decide which instrument the journal's operations point at.
-- +goose StatementBegin
DO $$
DECLARE
    duplicated TEXT;
BEGIN
    SELECT string_agg(d.isin || ' (' || d.n || ' instruments: ' || d.rows || ')', ', ' ORDER BY d.isin)
      INTO duplicated
      FROM (
          SELECT isin,
                 count(*) AS n,
                 string_agg(id::text || ' "' || name || '"', ', ' ORDER BY created_at, id) AS rows
            FROM instruments
           WHERE isin <> ''
           GROUP BY isin
          HAVING count(*) > 1
      ) d;

    IF duplicated IS NOT NULL THEN
        RAISE EXCEPTION '%',
            E'this database holds several instruments under the same ISIN, and this migration makes ISINs unique. Nothing has been changed.\n' ||
            E'Duplicated: ' || duplicated || E'.\n' ||
            E'An ISIN identifies a security worldwide, so rows sharing one are the same paper entered twice — and the two are already being told apart by nothing: whichever was created first is the one every import resolves to.\n' ||
            E'Fix the catalog, then start the application again. For each ISIN above: keep one instrument, move any operations off the others, and either delete those or clear their ISIN.';
    END IF;
END
$$;
-- +goose StatementEnd

CREATE UNIQUE INDEX instruments_isin_uniq ON instruments (isin) WHERE isin <> '';

DROP INDEX instruments_ticker_uniq;

CREATE UNIQUE INDEX instruments_ticker_uniq ON instruments (ticker)
    WHERE ticker <> '' AND isin = '' AND type IN ('share', 'bond', 'etf');

-- +goose Down
DROP INDEX instruments_isin_uniq;

-- The old rule can only be restored where the data still satisfies it; a
-- database that has since catalogued two papers under one ticker cannot go
-- back, and CREATE UNIQUE INDEX says so in its own words.
DROP INDEX instruments_ticker_uniq;
CREATE UNIQUE INDEX instruments_ticker_uniq ON instruments (ticker)
    WHERE ticker <> '' AND type IN ('share', 'bond', 'etf');
