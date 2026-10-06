-- +goose Up
-- Where a space's full valuation starts (decision Р-11): `liquid` counts only
-- what the market prices now, `nav` adds funds' net asset value, and
-- `nav_and_foreign` also foreign shares' prices on their home exchanges.
ALTER TABLE spaces ADD COLUMN full_valuation TEXT NOT NULL DEFAULT 'nav_and_foreign'
    CHECK (full_valuation IN ('liquid', 'nav', 'nav_and_foreign'));

-- +goose Down
ALTER TABLE spaces DROP COLUMN full_valuation;
