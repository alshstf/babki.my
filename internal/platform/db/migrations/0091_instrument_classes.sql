-- +goose Up
-- A paper's country of risk and sector (decision Р-46): where the business
-- is, not where the paper was issued, and what it does, in the program's own
-- list; from the broker's passport, else from Yahoo. A row with neither is a
-- paper the source did not know, asked again a month later.
CREATE TABLE instrument_classes (
    instrument_id uuid PRIMARY KEY REFERENCES instruments(id) ON DELETE CASCADE,
    country       text NOT NULL DEFAULT '' CHECK (country = '' OR country ~ '^[A-Z]{2}$'),
    sector        text NOT NULL DEFAULT '',
    source        text NOT NULL CHECK (source IN ('tinvest', 'yahoo')),
    raw           text NOT NULL DEFAULT '',
    fetched_at    timestamptz NOT NULL
);

-- +goose Down
DROP TABLE instrument_classes;
