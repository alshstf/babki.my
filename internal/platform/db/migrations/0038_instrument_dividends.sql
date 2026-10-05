-- +goose Up
-- The dividends an issuer declared, per share, as a market data source
-- publishes them (decision Р-14). They are what a foreign dividend's tax
-- withheld abroad is estimated from: the broker credits a payment already net
-- of that tax and says nothing of the gross, and the declared amount per share
-- times the shares held on the record date is the gross.
--
-- A source's calendar for one paper is replaced whole on every refresh, so
-- nothing here is keyed by the source's own identifiers: a dividend the source
-- corrects or withdraws is corrected or withdrawn here at the next refresh.
CREATE TABLE instrument_dividends (
    instrument_id UUID NOT NULL REFERENCES instruments(id) ON DELETE CASCADE,
    source        TEXT NOT NULL CHECK (source <> ''),
    record_date   DATE NOT NULL,
    payment_date  DATE,
    last_buy_date DATE,
    per_share     NUMERIC(28,9) NOT NULL CHECK (per_share > 0),
    currency      TEXT NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    fetched_at    TIMESTAMPTZ NOT NULL
);
CREATE INDEX instrument_dividends_instrument_idx ON instrument_dividends (instrument_id, record_date);

-- +goose Down
DROP TABLE instrument_dividends;
