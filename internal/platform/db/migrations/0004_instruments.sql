-- +goose Up
CREATE TABLE instruments (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    type             TEXT NOT NULL CHECK (type IN ('share','bond','etf','currency','crypto','metal','custom')),
    name             TEXT NOT NULL,
    ticker           TEXT NOT NULL DEFAULT '',
    isin             TEXT NOT NULL DEFAULT '',
    figi             TEXT NOT NULL DEFAULT '',
    currency         TEXT NOT NULL,
    face_value_minor BIGINT,
    face_currency    TEXT,
    frozen           BOOLEAN NOT NULL DEFAULT false,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX instruments_name_idx ON instruments (name);
CREATE INDEX instruments_ticker_idx ON instruments (ticker);

-- +goose Down
DROP TABLE instruments;
