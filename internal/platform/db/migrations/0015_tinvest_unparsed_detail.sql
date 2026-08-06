-- +goose Up
-- What refused a broker operation, in the refuser's own words, beside the code
-- that says which refusal it was.
--
-- WHY A SECOND COLUMN AND NOT A LONGER FIRST ONE. unparsed_reason is a CLOSED
-- SET: every value in it is declared in the contract (TinvestUnparsedReason in
-- api/openapi.yaml), the interface picks the Russian sentence the owner reads
-- by that value alone, and adding a member is a change both sides see. That is
-- what makes the code safe to compute from — and what makes it useless for
-- saying anything about ONE row. The detail is the opposite kind of thing:
-- free text written by whoever refused (the journal's own validation, the
-- portfolio engine replaying an account, the resolver failing to match a
-- security), never enumerated, never translated, and meant for the person
-- looking at one row and asking "yes, but which security, and how much of it".
-- Widening the code to carry both would end with the interface matching on
-- prose, which is the failure this project has been bitten by before.
--
-- AN EMPTY DETAIL IS A LEGITIMATE STATE, not a missing value, which is why the
-- column is NOT NULL DEFAULT '' rather than nullable: '' means "nothing was
-- written down", and a NULL would offer a second way to say the same thing
-- that no reader could tell from the first.
--
-- Said exactly, because the tempting sentence is the false one: every refusal
-- this program makes TODAY carries a detail — all twelve of the projection's
-- own (see projection.go), the resolver's, and the journal's. So empty is not
-- "the code already says it all"; it is the state of a row nobody has ruled on
-- since this column existed. The column still does not promise a detail, and
-- nothing may treat an empty one as an error, because the promise would be
-- broken by the first refusal that has nothing to add.
--
-- EVERY ROW ALREADY IN THE MIRROR GETS AN EMPTY DETAIL AND KEEPS IT UNTIL IT
-- IS RULED ON AFRESH. Old refusals are not rewritten retroactively and cannot
-- be: the text that would have gone here went to a log line and is gone. The
-- next rebuild of a connection ruling a row unreadable again fills it in
-- (Rebuilder.projectAll and Rebuilder.apply state a verdict for every row they
-- read), so the backfill is a re-import rather than an UPDATE in this file —
-- and inventing a detail here would be inventing the very thing the column
-- exists to stop being invented.
--
-- No index. Nothing looks a row up by this text, and nothing may: the unparsed
-- list is filtered and counted by unparsed_reason <> '', which the partial
-- index from 0014 already covers.
ALTER TABLE tinvest_operations_mirror
    ADD COLUMN unparsed_detail TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tinvest_operations_mirror
    DROP COLUMN unparsed_detail;
