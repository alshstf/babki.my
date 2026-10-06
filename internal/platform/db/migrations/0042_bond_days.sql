-- +goose Up
-- A bond's outstanding face value and the coupon interest accrued on it, per
-- unit and by day, as the exchange states them: a bond is worth its price in
-- percent of the current face plus that interest (plan 1.1, #190). Both are in
-- `currency`, the face's; interest stated in another currency is not kept.
CREATE TABLE bond_days (
    instrument_id UUID NOT NULL REFERENCES instruments(id) ON DELETE CASCADE,
    on_date       DATE NOT NULL,
    face_value    NUMERIC(30,10) NOT NULL CHECK (face_value > 0),
    accrued       NUMERIC(30,10) CHECK (accrued >= 0),
    currency      TEXT NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    source        TEXT NOT NULL CHECK (source <> ''),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (instrument_id, on_date)
);

-- +goose Down
DROP TABLE bond_days;
