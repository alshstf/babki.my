-- +goose Up
-- THE BROKER SAYS WHERE EVERY OPERATION HAPPENED AND THE PROGRAM THREW IT AWAY.
--
-- Every operation the gateway returns carries `classCode` — the trading mode
-- (режим торгов): the board an order was matched on, or the venue a deal was
-- struck off the order book. The mirror has kept it all along inside `raw`, so
-- nothing was lost, but no column held it, no journal row carried it and no
-- screen showed it — and so a purchase made off-exchange was indistinguishable
-- from one made on the exchange.
--
-- It is not a curiosity. After trading in foreign papers was suspended, the
-- broker opened over-the-counter dealing, and on the owner's own account
-- eleven operations in the FinEx funds (2024-08-21 … 2026-08-18) carry
-- `FINEX_OTC` while the rest carry ordinary Moscow Exchange boards. Which of
-- his lots were bought where is a question he asked directly, and the answer
-- was sitting in a jsonb column nobody read.
--
-- TWO COLUMNS, TWO DIFFERENT THINGS.
--
-- tinvest_operations_mirror.class_code is the broker's own field under the
-- broker's own name, because the mirror is a copy of what the broker said. The
-- backfill reads the payload the mirror already stored, so an existing
-- installation recovers the mode of every operation it ever imported without
-- asking the broker again — the property the mirror exists for, and the same
-- shape migration 0019 used for the ticker.
--
-- operations.trading_mode is the journal's own, named in this program's words,
-- and it is deliberately NOT backfilled from anywhere. The journal is a
-- projection of the mirror: the importer recomputes the whole desired journal
-- and diffs it against what is stored (see Rebuilder.difference and
-- sameJournalRow), so every imported row gets its mode on the next sync by the
-- one path that writes imported rows at all. A backfill here would be a second
-- writer of the same column, and the two would answer differently the first
-- time a rule changed.
--
-- NULL means "nothing said it", and that is a real state rather than a gap to
-- be filled: a hand-entered operation has no trading mode, and neither has one
-- the broker sent without a classCode (cash in and out of the account carry
-- none — 83 deposits and 52 withdrawals on the owner's account, where the
-- field describes no instrument and the broker leaves it empty).
--
-- content_key is deliberately NOT rebuilt to include the new column, for the
-- reason 0016 states at length: the key identifies an operation across syncs,
-- and changing its formula would make every row already in the mirror look
-- like one that disappeared and a new one that arrived.
ALTER TABLE tinvest_operations_mirror
    ADD COLUMN class_code TEXT NOT NULL DEFAULT '';

UPDATE tinvest_operations_mirror
   SET class_code = raw ->> 'classCode'
 WHERE raw ? 'classCode'
   AND raw ->> 'classCode' IS NOT NULL;

ALTER TABLE operations
    ADD COLUMN trading_mode TEXT;

-- +goose Down
ALTER TABLE operations
    DROP COLUMN trading_mode;

ALTER TABLE tinvest_operations_mirror
    DROP COLUMN class_code;
