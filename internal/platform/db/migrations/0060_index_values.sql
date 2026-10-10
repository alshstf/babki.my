-- +goose Up
-- Benchmark indices' daily closes (#401): total-return indices the family's
-- return is weighed against — the exchange's MCFTR and RGBITR, the S&P 500
-- with dividends. Shared by every space, like quotes.
CREATE TABLE index_values (
    code       text NOT NULL,
    on_date    date NOT NULL,
    value      numeric(24,6) NOT NULL CHECK (value > 0),
    source     text NOT NULL,
    fetched_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (code, on_date)
);

-- +goose Down
DROP TABLE index_values;
