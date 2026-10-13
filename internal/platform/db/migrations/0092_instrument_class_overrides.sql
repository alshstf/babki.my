-- +goose Up
-- A family's own country of risk or sector for a paper (decision Р-46), over
-- the source's: the broker files a bond fund as municipal shares, a group's
-- bond under its issuing subsidiary. Null keeps the source's.
CREATE TABLE instrument_class_overrides (
    space_id      uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    instrument_id uuid NOT NULL REFERENCES instruments(id) ON DELETE CASCADE,
    country       text CHECK (country IS NULL OR country ~ '^[A-Z]{2}$'),
    sector        text,
    PRIMARY KEY (space_id, instrument_id)
);

-- +goose Down
DROP TABLE instrument_class_overrides;
