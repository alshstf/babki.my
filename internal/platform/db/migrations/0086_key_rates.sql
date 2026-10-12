-- +goose Up
-- The Bank of Russia's key rate by day, percent: the year's highest sets how
-- much deposits' interest is free of tax (1 млн × the rate, НК РФ ст. 214.2;
-- decision Р-41).
CREATE TABLE key_rates (
    on_date    DATE PRIMARY KEY,
    rate       NUMERIC(6,2) NOT NULL CHECK (rate > 0),
    source     TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE key_rates;
