-- +goose Up
-- The five tables behind the T-Invest "mirror and projection" import (see the
-- design spec, 2026-08-04). tinvest_connections holds one read-only broker
-- token per space; tinvest_account_links ties it to the babki accounts it
-- feeds; tinvest_operations_mirror is the append-only copy of what the broker
-- says about each operation; tinvest_instrument_map resolves broker
-- instrument ids to this instance's shared catalog; tinvest_sync_runs is the
-- log a sync writes to and the reconciler reads from.
--
-- Deleting a connection cascades to every one of the other four tables below
-- it: links, mirror rows, the instrument map and sync runs. That is on
-- purpose and it is the ONLY thing "delete a connection" is allowed to reach.
-- It must NOT reach the babki accounts the connection fed or the journal
-- operations the projection wrote from the mirror: neither carries a foreign
-- key back to tinvest_connections, and that absence is deliberate — an
-- account is a first-class thing that may go on holding manually-entered
-- operations after the connection is gone, and a journal operation's only
-- link back to the mirror is the string external_id on its row. Severing that
-- string when the mirror row disappears IS what "the connection to the broker
-- breaks" means; the operation itself is not collateral.
--
-- "Mirror rows are never deleted" is a rule the sync worker follows (it only
-- ever adds a row or marks one disappeared_at) — it is not a promise that the
-- owner can never delete their own connection. Once the connection they
-- authorized is gone there is nothing left to mirror, so the cascade here and
-- that rule do not conflict.
CREATE TABLE tinvest_connections (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    space_id         UUID NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    status           TEXT NOT NULL DEFAULT 'active'
                     CHECK (status IN ('active','token_revoked','disabled')),
    token_ciphertext BYTEA NOT NULL,          -- AES-GCM: nonce||ciphertext
    token_last4      TEXT  NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- No last_sync_at column: "last successful sync" is derived from tinvest_sync_runs
-- (two independent computations of one value will diverge one day).

-- account_id is UNIQUE: a babki account fed by two links would make "which
-- link owns this account's operations" ambiguous, so one account takes at
-- most one link.
--
-- DELETING THE ACCOUNT REACHES FURTHER THAN THE LINK. The cascade on
-- account_id takes the link, and the two tables below that reference the link
-- cascade in their turn: every mirror row filed under it and every sync run
-- recorded for it. So deleting one babki account destroys this program's whole
-- copy of what the broker said about that account, and the history of every
-- sync of it — including the rows this file's opening note calls never
-- deleted, which is a rule about what the SYNC WORKER does and not a promise
-- the schema makes to a DELETE issued from anywhere else. What survives is the
-- connection itself, its other links, and everything filed under them.
--
-- Nothing in this program does that today: the DELETE endpoint for an account
-- ARCHIVES it (account.Handler.handleArchive), and no statement anywhere
-- deletes an accounts row. It is written down for the day somebody adds one,
-- because at that point the choice is between re-mirroring the account's whole
-- history from the broker and not being able to.
CREATE TABLE tinvest_account_links (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    connection_id       UUID NOT NULL REFERENCES tinvest_connections(id) ON DELETE CASCADE,
    space_id            UUID NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    account_id          UUID NOT NULL UNIQUE REFERENCES accounts(id) ON DELETE CASCADE,
    broker_account_id   TEXT NOT NULL,
    broker_account_name TEXT NOT NULL,
    broker_account_type TEXT NOT NULL,
    opened_on           DATE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (connection_id, broker_account_id)
);

CREATE TABLE tinvest_operations_mirror (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),  -- becomes the journal operation's external_id when projected
    connection_id       UUID NOT NULL REFERENCES tinvest_connections(id) ON DELETE CASCADE,
    link_id             UUID NOT NULL REFERENCES tinvest_account_links(id) ON DELETE CASCADE,
    broker_operation_id TEXT NOT NULL,            -- attribute, NOT a key (broker ids drift)
    parent_operation_id TEXT NOT NULL DEFAULT '',
    op_type             TEXT NOT NULL,
    state               TEXT NOT NULL,
    occurred_at         TIMESTAMPTZ NOT NULL,
    currency            TEXT NOT NULL,            -- normalized UPPER
    payment             NUMERIC(28,9) NOT NULL,   -- broker units+nano, exact copy
    price               NUMERIC(28,9),
    commission          NUMERIC(28,9),
    commission_currency TEXT NOT NULL DEFAULT '',
    accrued_int         NUMERIC(28,9),
    quantity            BIGINT NOT NULL DEFAULT 0,
    figi                TEXT NOT NULL DEFAULT '',
    instrument_uid      TEXT NOT NULL DEFAULT '',
    position_uid        TEXT NOT NULL DEFAULT '',
    asset_uid           TEXT NOT NULL DEFAULT '',
    instrument_type     TEXT NOT NULL DEFAULT '',
    description         TEXT NOT NULL DEFAULT '',
    raw                 JSONB NOT NULL,
    content_key         TEXT NOT NULL,            -- computed ONCE at write, never rebuilt from these columns
    first_seen_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_confirmed_at   TIMESTAMPTZ NOT NULL,
    disappeared_at      TIMESTAMPTZ,              -- NULL = broker still returns it
    unparsed_reason     TEXT NOT NULL DEFAULT ''  -- '' = projected
);
CREATE INDEX tinvest_mirror_link_key_idx ON tinvest_operations_mirror (link_id, content_key);
CREATE INDEX tinvest_mirror_unparsed_idx ON tinvest_operations_mirror (connection_id)
    WHERE unparsed_reason <> '';
-- content_key is NOT unique: the mirror is a multiset. Two broker operations
-- can legitimately be identical in every column that feeds content_key — one
-- instrument, one second, one amount (e.g. two identical top-ups made in the
-- same minute) — and both are real operations that have to exist as their
-- own row. A unique index on content_key would not merge them: Postgres
-- would reject the second INSERT outright with a uniqueness violation, and
-- that rejected insert is how a real, distinct operation would be lost.

CREATE TABLE tinvest_instrument_map (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    connection_id  UUID NOT NULL REFERENCES tinvest_connections(id) ON DELETE CASCADE,
    instrument_id  UUID NOT NULL REFERENCES instruments(id) ON DELETE CASCADE,
    figi           TEXT NOT NULL DEFAULT '',
    instrument_uid TEXT NOT NULL DEFAULT '',
    position_uid   TEXT NOT NULL DEFAULT '',
    asset_uid      TEXT NOT NULL DEFAULT '',
    isin           TEXT NOT NULL DEFAULT '',
    ticker         TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (connection_id, instrument_uid)
);
CREATE INDEX tinvest_map_figi_idx ON tinvest_instrument_map (connection_id, figi);

CREATE TABLE tinvest_sync_runs (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    connection_id        UUID NOT NULL REFERENCES tinvest_connections(id) ON DELETE CASCADE,
    link_id              UUID NOT NULL REFERENCES tinvest_account_links(id) ON DELETE CASCADE,
    trigger              TEXT NOT NULL CHECK (trigger IN ('schedule','manual','initial')),
    status               TEXT NOT NULL CHECK (status IN ('running','ok','failed')),
    started_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at          TIMESTAMPTZ,
    read_count           INT NOT NULL DEFAULT 0,
    added_count          INT NOT NULL DEFAULT 0,
    disappeared_count    INT NOT NULL DEFAULT 0,
    unparsed_count       INT NOT NULL DEFAULT 0,
    error                TEXT NOT NULL DEFAULT '',
    reconcile_status     TEXT NOT NULL DEFAULT 'not_checked'
                         CHECK (reconcile_status IN ('not_checked','matched','mismatched')),
    reconciled_at        TIMESTAMPTZ,
    reconcile_mismatches JSONB
);
CREATE INDEX tinvest_runs_conn_idx ON tinvest_sync_runs (connection_id, started_at DESC);

-- +goose Down
DROP TABLE tinvest_sync_runs;
DROP TABLE tinvest_instrument_map;
DROP TABLE tinvest_operations_mirror;
DROP TABLE tinvest_account_links;
DROP TABLE tinvest_connections;
