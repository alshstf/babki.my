-- +goose Up
-- A bond's schedule as the exchange publishes it (#399, the payouts calendar):
-- coupons, partial repayments, the redemption and offers, per unit of the
-- bond, in the face's currency. A floating coupon not yet set has no value.
-- A source's schedule for one bond is replaced whole on every refresh.
CREATE TABLE bond_events (
    instrument_id UUID NOT NULL REFERENCES instruments(id) ON DELETE CASCADE,
    source        TEXT NOT NULL CHECK (source <> ''),
    kind          TEXT NOT NULL CHECK (kind IN ('coupon', 'amortization', 'redemption', 'offer')),
    on_date       DATE NOT NULL,
    record_date   DATE,
    value         NUMERIC(30,10) CHECK (value >= 0),
    percent       NUMERIC(20,10),
    currency      TEXT NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    fetched_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (instrument_id, source, kind, on_date)
);

-- +goose Down
DROP TABLE bond_events;
