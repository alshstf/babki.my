-- +goose Up
-- What the owner knows about a broker operation that this program cannot know.
--
-- The broker sends no corporate actions at all — its operation enum has no
-- split, no conversion, no spin-off — so a real event reaches the mirror
-- disguised as whatever type happened to carry the money. On the owner's own
-- account Т-Капитал redeemed 73 % of the units of one fund and sent it as two
-- unrelated rows a fortnight apart: an OUTPUT_SECURITIES of 44 380,35 units
-- ("вывод в другой депозитарий") and a BOND_REPAYMENT_FULL of 2 559,80 ₽. No
-- rule over those two rows can find the one partial redemption they are; the
-- owner can, and this table is where that answer is kept.
--
-- A row named here is not projected at all: no journal entries, and not
-- unparsed either — the manual operation named by operation_id is what stands
-- for the event, and the mirror row is marked as accounted for by hand.
--
-- IDENTITY IS content_key, NOT THE BROKER'S OPERATION ID. The broker's own
-- documentation says an operation's id may change over time and its history is
-- rewritten in place; content_key is what this mirror is keyed by for that
-- reason (see MirrorRow and contentKey), and an explanation must survive the
-- same rewriting the row it explains survives. It is not a foreign key onto
-- the mirror's own id for the same reason a re-sync may replace the row while
-- the content stays: the key is the content.
--
-- ON DELETE CASCADE from operations is the whole un-explaining rule. Deleting
-- the manual operation — from the journal screen, or by this connection being
-- removed — takes its explanations with it, and the rows it covered go back to
-- being projected on the next rebuild. There is no second path that could
-- leave an explanation pointing at an operation that is gone.
CREATE TABLE tinvest_mirror_explanations (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    link_id      UUID NOT NULL REFERENCES tinvest_account_links (id) ON DELETE CASCADE,
    content_key  TEXT NOT NULL,
    operation_id UUID NOT NULL REFERENCES operations (id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- One explanation per mirror row: a row is accounted for by hand once, or
    -- not at all. Several rows may name ONE operation — the two halves of the
    -- fund redemption above are explained together — which is why the
    -- uniqueness is on the row and not on the operation.
    UNIQUE (link_id, content_key)
);

-- The projection asks this table one question per link, on every rebuild:
-- which of this link's rows are spoken for.
CREATE INDEX tinvest_explanations_link_idx ON tinvest_mirror_explanations (link_id);

-- And the listing asks it the other way round: which explanations name this
-- operation, so that deleting one from the journal can say what it will
-- un-explain.
CREATE INDEX tinvest_explanations_operation_idx ON tinvest_mirror_explanations (operation_id);

-- +goose Down
DROP TABLE tinvest_mirror_explanations;
