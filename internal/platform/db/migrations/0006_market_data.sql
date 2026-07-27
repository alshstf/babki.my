-- +goose Up
ALTER TABLE spaces ADD COLUMN base_currency TEXT NOT NULL DEFAULT 'RUB';

-- Daily FX rates: how many `quote` units per 1 `base` unit.
CREATE TABLE fx_rates (
    base       TEXT NOT NULL,
    quote      TEXT NOT NULL,
    on_date    DATE NOT NULL,
    rate       NUMERIC(30,10) NOT NULL CHECK (rate > 0),
    source     TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (base, quote, on_date)
);
CREATE INDEX fx_rates_lookup_idx ON fx_rates (base, quote, on_date DESC);

CREATE TABLE quotes (
    instrument_id UUID NOT NULL REFERENCES instruments(id) ON DELETE CASCADE,
    on_date       DATE NOT NULL,
    price         NUMERIC(30,10) NOT NULL CHECK (price >= 0),
    currency      TEXT NOT NULL,
    source        TEXT NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (instrument_id, on_date)
);
CREATE INDEX quotes_lookup_idx ON quotes (instrument_id, on_date DESC);

-- +goose Down
DROP TABLE quotes;
DROP TABLE fx_rates;
ALTER TABLE spaces DROP COLUMN base_currency;
