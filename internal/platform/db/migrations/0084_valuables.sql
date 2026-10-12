-- +goose Up
-- Physical metals (decisions Р-39 А, Р-40 Г): an account of the new type
-- `valuables` is where bars and coins are kept — at home, in a bank's safe
-- box, in a bank's custody.
ALTER TABLE accounts DROP CONSTRAINT accounts_type_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_type_check
    CHECK (type IN ('brokerage','checking','savings','deposit','credit_card','loan','cash','valuables'));

-- The place: where, with whom, until when and for how much; and what below
-- the central bank's price the bank would buy the metal back for (null: the
-- program's guide by metal and weight).
CREATE TABLE valuables_storages (
    account_id      uuid PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    place           text NOT NULL CHECK (place IN ('home', 'safe_box', 'custody', 'other')),
    bank            text NOT NULL DEFAULT '',
    until           date,
    fee_minor       bigint CHECK (fee_minor >= 0),
    fee_period      text CHECK (fee_period IN ('month', 'year')),
    buyback_percent numeric(5,2) CHECK (buyback_percent >= 0 AND buyback_percent < 100),
    CHECK ((fee_minor IS NULL) = (fee_period IS NULL))
);

-- A bar or a coin, each on its own: its tax date and whether a bank takes it
-- back without an assay are its own.
CREATE TABLE valuables_items (
    id          uuid PRIMARY KEY,
    account_id  uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    kind        text NOT NULL CHECK (kind IN ('bar', 'coin')),
    metal       text NOT NULL CHECK (metal IN ('gold', 'silver', 'platinum', 'palladium')),
    weight      numeric(12,4) NOT NULL CHECK (weight > 0),
    fineness    numeric(5,1) NOT NULL CHECK (fineness > 0 AND fineness <= 1000),
    maker       text NOT NULL DEFAULT '',
    foreign_made boolean NOT NULL DEFAULT false,
    serial      text NOT NULL DEFAULT '',
    certificate boolean NOT NULL DEFAULT true,
    sealed      boolean NOT NULL DEFAULT true,
    bought_on   date NOT NULL,
    cost_minor  bigint NOT NULL CHECK (cost_minor >= 0),
    currency    text NOT NULL,
    owner_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
    note        text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX valuables_items_account_idx ON valuables_items (account_id);

-- What the family's bank buys a gram back for, as written in by hand: it
-- stands for the guide while it is no older than a month (Р-40 Г).
CREATE TABLE valuables_bank_prices (
    account_id uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    metal      text NOT NULL CHECK (metal IN ('gold', 'silver', 'platinum', 'palladium')),
    price      numeric(18,4) NOT NULL CHECK (price > 0),
    on_date    date NOT NULL,
    PRIMARY KEY (account_id, metal)
);

-- +goose Down
DROP TABLE valuables_bank_prices;
DROP TABLE valuables_items;
DROP TABLE valuables_storages;
DELETE FROM accounts WHERE type = 'valuables';
ALTER TABLE accounts DROP CONSTRAINT accounts_type_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_type_check
    CHECK (type IN ('brokerage','checking','savings','deposit','credit_card','loan','cash'));
