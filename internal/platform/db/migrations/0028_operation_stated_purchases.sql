-- +goose Up
-- What the owner stated about the purchases behind shares that arrived from
-- another broker without them: an imported transfer_in with no sibling.
--
-- The journal row carries these pieces as its breakdown, like any transfer.
-- They are kept here as well because an importer rebuilds its rows from the
-- broker's record on every sync, and the broker's record does not have them:
-- this is where the importer finds them again. Keyed by the row's own name
-- (source, external_id) within its account, which is how an importer names the
-- rows it writes. A hand-entered arrival needs no such copy — nothing rebuilds
-- it — so 'manual' is not a source here.
CREATE TABLE operation_stated_purchases (
    space_id    UUID NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    account_id  UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    source      TEXT NOT NULL CHECK (source <> 'manual'),
    external_id TEXT NOT NULL,
    seq         INT NOT NULL CHECK (seq >= 0),
    quantity    NUMERIC(30,10) NOT NULL CHECK (quantity > 0),
    cost_minor  BIGINT NOT NULL CHECK (cost_minor >= 0),
    acquired_on DATE,
    PRIMARY KEY (account_id, source, external_id, seq)
);

-- +goose Down
DROP TABLE operation_stated_purchases;
