-- +goose Up
-- The broker's last-price answer carries no currency at all — a figure, an
-- instant and the instrument's own identifier, nothing more. What the figure is
-- denominated in is a property of the LISTING the price came from, and one
-- security has several: Apple is quoted in dollars on СПБ and a FinEx fund in
-- rubles on the broker's own over-the-counter line, while the same catalog row
-- stands behind either. Taking the currency from the catalog row would
-- therefore stamp a price with a currency that belongs to a different listing
-- of the same paper — the silent corruption this program refuses everywhere
-- else in the money path.
--
-- So it is recorded per broker instrument, where the answer actually lives. It
-- comes from the instrument's own passport, which the resolver already fetches
-- for every mapping it creates.
--
-- Empty means "not learned yet", which is what every existing row starts as:
-- the passports are not re-fetched here, because a migration must not depend on
-- a network. The quotes worker fills them in as it goes and prices nothing it
-- has no currency for.
ALTER TABLE tinvest_instrument_map
    ADD COLUMN currency TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tinvest_instrument_map DROP COLUMN currency;
