-- +goose Up
-- One instrument per ticker, among the instruments a price is ever fetched for.
--
-- The quotes job maps ticker -> instrument id (see marketdata.quotesWorker) and
-- has to: the provider answers with tickers and knows nothing about this
-- catalog. A map holds one value per key, so two catalog rows carrying the same
-- ticker meant one of them was overwritten and never priced again — no error,
-- no log line, just a position showing no quote forever. The map was already
-- assuming what this index now makes true.
--
-- PARTIAL, over exactly the rows that reach that map: instrument.ListTradable
-- selects `type IN ('share','bond','etf') AND ticker <> ''`, and the predicate
-- below is that same filter. It is not one character wider on purpose. Rows
-- outside it are never looked up by ticker, so uniqueness there would refuse
-- writes that cost nobody a price: the create dialog offers all seven
-- instrument types with a free ticker field, so two `crypto` rows for one coin
-- held on two venues, or two `metal` rows both reading XAU for gold vaulted at
-- two brokers, are things a user legitimately has. Rows with no ticker at all
-- are outside it for the same reason — the empty string is how "this instrument
-- has no exchange ticker" is written down (cash, metals, hand-made holdings),
-- and any number of them may exist. All of that worked before this migration
-- and still does.
--
-- The predicate and ListTradable's WHERE are now two spellings of one rule,
-- which is the shape that drifts apart later. They are held together by
-- instrument.TestUniqueTickerCoversExactlyTheRowsListTradableReturns: it asks
-- the catalog for a duplicated ticker under every instrument type there is, and
-- compares refusal against what the reader actually returned, so widening
-- either side alone turns it red.
--
-- The ticker ALONE, with no exchange beside it. That is a real limit: the same
-- ticker on two exchanges is one instrument here. It is also the limit the code
-- already had — the whole provider interface speaks in bare tickers — and the
-- honest place to state a limit is at the write that breaks it, not at a silent
-- drop three layers later. Keying on (exchange, ticker) instead would mean the
-- provider had to say which exchange each price came from, which is a different
-- piece of work, not a line of SQL.

-- Duplicates already in the database stop the upgrade, with a message that says
-- what to do about them. The alternative — letting CREATE UNIQUE INDEX fail on
-- its own — reports "could not create unique index instruments_ticker_uniq",
-- which names a Postgres object rather than the problem. Merging the rows
-- instead is not on the table: choosing which of two catalog entries is the real
-- one decides which instrument the journal's operations point at, and the
-- journal is the source of truth here. No migration edits it unattended.
--
-- The report names every duplicate by id and by name, not by ticker alone: the
-- operator has to open those rows to decide which one to keep, and a bare
-- ticker leaves them hunting for the rows first.
--
-- The WHERE below repeats the index predicate, and must — it reports on exactly
-- the rows the index is about to refuse. A wider query stops an upgrade the
-- index would have allowed, and
-- db.TestMigrate_DuplicatesOutsideTheTradableSetDoNotStopTheUpgrade turns red
-- on it. A narrower one leaves CREATE UNIQUE INDEX to fail on its own, and the
-- operator gets the Postgres sentence instead of the message
-- db.TestMigrate_DuplicateTickersStopTheUpgradeAndSayWhatToDo asserts on — that
-- test stages shares, so a narrowing that drops only bonds or funds would slip
-- past it.
--
-- Everything the operator needs is in the MESSAGE, not in DETAIL or HINT:
-- Postgres carries those as separate fields and pgconn.PgError.Error() prints
-- neither, so text put there would never reach the console.
-- +goose StatementBegin
DO $$
DECLARE
    duplicated TEXT;
BEGIN
    SELECT string_agg(d.ticker || ' (' || d.n || ' instruments: ' || d.rows || ')', ', ' ORDER BY d.ticker)
      INTO duplicated
      FROM (
          SELECT ticker,
                 count(*) AS n,
                 string_agg(id::text || ' "' || name || '"', ', ' ORDER BY created_at, id) AS rows
            FROM instruments
           WHERE ticker <> '' AND type IN ('share', 'bond', 'etf')
           GROUP BY ticker
          HAVING count(*) > 1
      ) d;

    IF duplicated IS NOT NULL THEN
        -- Assembled with || rather than as a run of adjacent literals: only an
        -- E'' string reads \n as a line break, and the lexer does not
        -- concatenate a run of those.
        RAISE EXCEPTION '%',
            E'this database holds several instruments under the same ticker, and this migration makes tickers unique. Nothing has been changed.\n' ||
            E'Duplicated: ' || duplicated || E'.\n' ||
            E'Only one instrument per ticker can ever be priced, because the quotes job looks instruments up by ticker. So of every group above one is priced today and the rest silently show no quote — that is the bug this migration closes.\n' ||
            E'Fix the catalog, then start the application again. For each ticker above: keep one instrument, move any operations off the others, and either delete those or clear their ticker. An instrument with an empty ticker is kept as it is; it is simply never priced.\n' ||
            E'Only shares, bonds and funds are listed, because prices are fetched for those alone: two crypto, metal, currency or custom instruments may go on sharing a ticker, and nothing above concerns them.';
    END IF;
END
$$;
-- +goose StatementEnd

CREATE UNIQUE INDEX instruments_ticker_uniq ON instruments (ticker)
    WHERE ticker <> '' AND type IN ('share', 'bond', 'etf');

-- instruments_ticker_idx (migration 0004) goes: nothing looks an instrument up
-- by ticker equality or by prefix. Search matches ILIKE '%fragment%', which
-- leads with a wildcard, and no btree can serve that. The one query left that
-- names the column is ListTradable, and the index just created holds precisely
-- the rows it asks for, in precisely the order it asks for them — its predicate
-- IS that query's WHERE — over a fraction of the table. So the plain index
-- offers that query nothing the partial one does not, and offers no other query
-- anything at all: what was left of it is the cost of keeping it up to date on
-- every write.
DROP INDEX instruments_ticker_idx;

-- +goose Down
DROP INDEX instruments_ticker_uniq;
CREATE INDEX instruments_ticker_idx ON instruments (ticker);
