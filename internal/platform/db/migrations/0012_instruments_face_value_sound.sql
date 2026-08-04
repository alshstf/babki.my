-- +goose Up
-- A face value is either a positive number of minor units WITH the currency it
-- is denominated in, or it is absent altogether. Nothing in between.
--
-- An EMPTY STRING is one of the things in between, and the reason it needs
-- saying is that it does not look like one: `'' IS NULL` is false, so a face
-- currency of '' satisfies a plain IS NULL equality while naming no currency at
-- all. That is a half pair wearing a value — the readers demand a currency and
-- get a string that denominates nothing, so the valuation comes out as a bare
-- number with nothing on it and the trade dialog writes «Номинал в , а сделка в
-- RUB». Written as a separate clause below rather than folded into the equality,
-- because the equality is the honest statement of the pairing rule and '' is a
-- second thing entirely.
--
-- What this deliberately does NOT do is enforce the ISO-4217 SHAPE. That rule
-- belongs at the door (currencyRe in internal/instrument/http.go), where it has
-- always been applied to the instrument's own currency column — which carries no
-- constraint here either. Spelling the alphabet out in SQL as well would make
-- face_currency the one currency in this schema whose shape is stated twice, in
-- two languages, with nothing keeping the two statements in step; and this
-- codebase's recurring bug is precisely two statements of one rule drifting
-- apart. What the database is asked for is the part the readers cannot survive
-- without and cannot check for themselves: that a face value which exists is
-- denominated in SOMETHING.
--
-- The pair is what turns a bond's quote into money. An exchange quotes a bond as
-- a PERCENTAGE OF FACE, not in money per unit (see portfolio.marketValue and
-- bondPriceFromPercent in the frontend), so face value is the factor the whole
-- valuation rests on: at zero, every quote there will ever be values the entire
-- holding at 0,00 — a number on the positions screen that is not the truth, and
-- the one thing this program refuses to publish. Half a pair is milder, because
-- every reader demands both halves before valuing anything, but it is still a
-- bond that silently cannot be priced.
--
-- All three states were reachable until #93: creation checked that the two
-- arrived together and never that the value was positive, so zero went in; it
-- checked the currency's presence and never its shape, so '' went in; and the
-- update checked nothing at all, so a PATCH could clear one half and leave the
-- other. Those two doors now refuse (checkFacePair / checkFaceUpdate in
-- internal/instrument/http.go), which is where a refusal belongs — it comes back
-- as a named field rather than as a screen that cannot explain itself.
--
-- THIS CONSTRAINT IS THE OTHER HALF OF THAT: the handler's rules make every
-- accepted write leave the pair whole, and this makes "every row is whole" true
-- rather than merely intended. That is what lets the readers stop deriving it
-- one at a time — the point of moving the check to the write in the first place.

-- Rows already in this state stop the upgrade, with a message that says what to
-- do about them. The alternative — letting ADD CONSTRAINT fail on its own —
-- reports `check constraint "instruments_face_value_sound" of relation
-- "instruments" is violated by some row`, which names a Postgres object and one
-- anonymous row rather than the problem and the instruments it is in.
--
-- Repairing them instead is not on the table, and for the same reason migration
-- 0011 refuses to merge duplicate tickers: choosing a face value decides what
-- every position in that bond is worth, and clearing the pair throws away a
-- number somebody typed. No migration makes either choice unattended.
--
-- Reaching this state at all takes deliberate use of the API: no screen writes a
-- face value (the catalog dialog has no such field) and nothing in the frontend
-- PATCHes an instrument. So this is expected never to fire; it costs one query
-- on upgrade and buys the invariant every reader from here on relies on.
--
-- Everything the operator needs is in the MESSAGE, not in DETAIL or HINT:
-- Postgres carries those as separate fields and pgconn.PgError.Error() prints
-- neither, so text put there would never reach the console.
-- +goose StatementBegin
DO $$
DECLARE
    unsound TEXT;
BEGIN
    SELECT string_agg(
               i.id::text || ' "' || i.name || '": face_value_minor = ' ||
               coalesce(i.face_value_minor::text, 'NULL') ||
               ', face_currency = ' || coalesce('"' || i.face_currency || '"', 'NULL'),
               E'\n  ' ORDER BY i.created_at, i.id)
      INTO unsound
      FROM instruments i
     WHERE (i.face_value_minor IS NULL) <> (i.face_currency IS NULL)
        OR i.face_value_minor <= 0
        OR i.face_currency = '';

    IF unsound IS NOT NULL THEN
        -- Assembled with || rather than as a run of adjacent literals: only an
        -- E'' string reads \n as a line break, and the lexer does not
        -- concatenate a run of those.
        RAISE EXCEPTION '%',
            E'this database holds instruments whose face value is not one, and this migration makes that state impossible. Nothing has been changed.\n' ||
            E'Affected:\n  ' || unsound || E'\n' ||
            E'A bond is quoted as a percentage of its face value, so the face value is what turns a quote into money. Zero or less values the whole holding at nothing, and a value without its currency — or a currency without its value — cannot value it at all.\n' ||
            E'Fix the catalog, then start the application again. For each instrument above either record both halves (a positive face_value_minor and the face_currency it is in), or clear both to NULL. An instrument with neither is kept as it is; it simply cannot be priced as a bond until a face value is recorded.\n' ||
            E'Nothing is repaired here on purpose: choosing a face value decides what every position in that bond is worth, and clearing the pair discards a number somebody entered. Neither is a migration''s decision to make.';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE instruments ADD CONSTRAINT instruments_face_value_sound
    CHECK ((face_value_minor IS NULL) = (face_currency IS NULL)
       AND (face_value_minor IS NULL OR face_value_minor > 0)
       AND (face_currency IS NULL OR face_currency <> ''));

-- +goose Down
ALTER TABLE instruments DROP CONSTRAINT instruments_face_value_sound;
