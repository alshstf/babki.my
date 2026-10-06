-- +goose Up
-- Prices a paper is valued at in the full valuation (decision Р-11), apart from
-- the quotes the liquid one uses: a fund's net asset value per unit (`nav`) and
-- a foreign share's price on its home exchange (`foreign`). Kept apart so that
-- neither can pass for a price the paper can be sold at here.
CREATE TABLE reference_prices (
    instrument_id UUID NOT NULL REFERENCES instruments(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL CHECK (kind IN ('nav', 'foreign')),
    on_date       DATE NOT NULL,
    price         NUMERIC(30,10) NOT NULL CHECK (price > 0),
    currency      TEXT NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    source        TEXT NOT NULL CHECK (source <> ''),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (instrument_id, kind, on_date)
);
CREATE INDEX reference_prices_lookup_idx ON reference_prices (instrument_id, kind, on_date DESC);

-- +goose Down
DROP TABLE reference_prices;
