-- +goose Up
-- One instrument per ticker, enforced.
--
-- The quotes job maps ticker -> instrument id (see marketdata.quotesWorker) and
-- has to: the provider answers with tickers and knows nothing about this
-- catalog. A map holds one value per key, so two catalog rows carrying the same
-- ticker meant one of them was overwritten and never priced again — no error,
-- no log line, just a position showing no quote forever. The map was already
-- assuming what this index now makes true.
--
-- PARTIAL, over the non-empty tickers only. The column is NOT NULL DEFAULT ''
-- and the empty string is how "this instrument has no exchange ticker" is
-- written down: cash, metals, hand-made holdings. Those rows are not comparable
-- to one another and are never looked up by ticker — instrument.ListTradable
-- excludes them explicitly — so a full unique index would forbid the second such
-- instrument for nothing. The predicate is character for character the one the
-- reader filters by, so every row that can reach the reader's map is covered.
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
-- Everything the operator needs is in the MESSAGE, not in DETAIL or HINT:
-- Postgres carries those as separate fields and pgconn.PgError.Error() prints
-- neither, so text put there would never reach the console.
-- +goose StatementBegin
DO $$
DECLARE
    duplicated TEXT;
BEGIN
    SELECT string_agg(d.ticker || ' (' || d.n || ' instruments)', ', ' ORDER BY d.ticker)
      INTO duplicated
      FROM (
          SELECT ticker, count(*) AS n
            FROM instruments
           WHERE ticker <> ''
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
            E'Fix the catalog, then start the application again. For each ticker above: keep one instrument, move any operations off the others, and either delete those or clear their ticker. An instrument with an empty ticker is kept as it is; it is simply never priced.';
    END IF;
END
$$;
-- +goose StatementEnd

CREATE UNIQUE INDEX instruments_ticker_uniq ON instruments (ticker) WHERE ticker <> '';

-- +goose Down
DROP INDEX instruments_ticker_uniq;
