-- +goose Up
-- The family's target shares of its worth (decision Р-37): by kind of asset
-- — what the suggestions bring the worth to — and by currency, for the
-- record only; percents of each dimension add up to 100.
CREATE TABLE allocation_targets (
    space_id  uuid         NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    dimension text         NOT NULL CHECK (dimension IN ('kind', 'currency')),
    key       text         NOT NULL,
    percent   numeric(5,2) NOT NULL CHECK (percent >= 0 AND percent <= 100),
    PRIMARY KEY (space_id, dimension, key)
);

-- How the family brings its worth to the targets: by top-ups only, or by
-- buying and selling.
CREATE TABLE allocation_settings (
    space_id uuid PRIMARY KEY REFERENCES spaces(id) ON DELETE CASCADE,
    mode     text NOT NULL CHECK (mode IN ('topup', 'rebalance'))
);

-- +goose Down
DROP TABLE allocation_settings;
DROP TABLE allocation_targets;
