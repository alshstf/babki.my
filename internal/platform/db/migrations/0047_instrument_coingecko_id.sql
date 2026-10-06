-- +goose Up
-- A cryptocurrency's coin at CoinGecko, which prices it (decision Р-20).
-- Tickers are not unique among coins (dozens are «BTC» something), so the coin
-- is kept once chosen: picked by the price job as the largest coin with the
-- ticker, and a person's to correct. Empty for anything else.
ALTER TABLE instruments ADD COLUMN coingecko_id text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE instruments DROP COLUMN coingecko_id;
