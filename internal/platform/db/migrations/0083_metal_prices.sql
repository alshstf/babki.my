-- +goose Up
-- The Bank of Russia's accounting prices of refined precious metals, roubles
-- per gram (physical metals, decisions Р-39 and Р-40): what a bar is worth by
-- the market. Apart from fx_rates, where XAU is the exchange's spot gold.
CREATE TABLE metal_prices (
    metal      TEXT NOT NULL CHECK (metal IN ('gold', 'silver', 'platinum', 'palladium')),
    on_date    DATE NOT NULL,
    price      NUMERIC(18,4) NOT NULL CHECK (price > 0),
    source     TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (metal, on_date)
);

-- +goose Down
DROP TABLE metal_prices;
