-- +goose Up
-- The space records the OWNER'S country of tax residency, as an ISO 3166-1
-- alpha-2 code. It decides which cost basis rules the application's figures
-- have to be judged against: which parcels a sale is deemed to consume (FIFO,
-- averaging, or the taxpayer's own pick) and over what holdings that queue is
-- built (one account, or everything the owner holds). Both are derived from
-- this one column through a single table in the code (see
-- internal/family/taxresidency.go), never inferred at the point of use.
--
-- IT IS ON THE SPACE, NOT ON THE ACCOUNT, and that is a statement about the
-- world rather than a schema convenience: residency belongs to the person, and
-- a Russian resident declares a foreign broker's account by Russian rules just
-- the same. A per-account country would let one family hold accounts that each
-- claim a different rulebook, which no tax authority recognises.
--
-- EXISTING SPACES BECOME 'RU'. That is not a placeholder standing in for "not
-- filled in": every space that exists today was created by this application's
-- only owner, who is a Russian resident, and Russia is exactly the FIFO /
-- per-account rule the engine has computed from the first commit. The default
-- therefore states what those spaces have always meant, and nothing about their
-- figures changes when they read it back. A NULL would have said "unknown" —
-- a claim that is both false here and would force every reader to invent an
-- answer, which is the failure this whole change exists to remove.
--
-- The CHECK constrains the SHAPE only, deliberately. The list of countries the
-- application actually has rules for lives in exactly one place, the Go table,
-- and repeating it here would create a second copy to keep in step — with a
-- migration required to add a country, and silent divergence the day someone
-- forgets. Which codes are ACCEPTED is enforced on the write path against that
-- one table (see family.Service.UpdateSpace); a code that somehow reaches this
-- column without a rules row is reported as unknown on the read path, never
-- quietly treated as Russia.
ALTER TABLE spaces
    ADD COLUMN tax_residency TEXT NOT NULL DEFAULT 'RU'
        CHECK (tax_residency ~ '^[A-Z]{2}$');

-- +goose Down
ALTER TABLE spaces DROP COLUMN tax_residency;
