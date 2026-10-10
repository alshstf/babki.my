-- +goose Up
-- A card whose grace runs in windows (decision Р-28, #446): purchases of a
-- few statement periods in a row, counted from the period the contract was
-- made in, stay free until the end of a later period — «180 дней»
-- Газпромбанка: two months of purchases, paid by the end of the sixth. And
-- three ways banks differ: a missed deadline that takes the grace off the
-- whole debt until it is repaid; the minimum due by the end of the next
-- period rather than some days after the statement; the interest and fees
-- charged paid in full on top of the minimum's share.
ALTER TABLE credit_cards DROP CONSTRAINT credit_cards_grace_kind_check;
ALTER TABLE credit_cards ADD CONSTRAINT credit_cards_grace_kind_check
    CHECK (grace_kind IN ('statement', 'long', 'windows'));
ALTER TABLE credit_cards
    ADD COLUMN window_months     smallint NOT NULL DEFAULT 0 CHECK (window_months BETWEEN 0 AND 12),
    ADD COLUMN grace_months      smallint NOT NULL DEFAULT 0 CHECK (grace_months BETWEEN 0 AND 36),
    ADD COLUMN opened_on         date,
    ADD COLUMN grace_all_lost    boolean NOT NULL DEFAULT false,
    ADD COLUMN pay_by_period_end boolean NOT NULL DEFAULT false,
    ADD COLUMN charges_in_full   boolean NOT NULL DEFAULT false;

-- +goose Down
-- A card in windows becomes a long grace of about as many days.
UPDATE credit_cards SET grace_kind = 'long', grace_days = grace_months * 30 WHERE grace_kind = 'windows';
ALTER TABLE credit_cards
    DROP COLUMN window_months,
    DROP COLUMN grace_months,
    DROP COLUMN opened_on,
    DROP COLUMN grace_all_lost,
    DROP COLUMN pay_by_period_end,
    DROP COLUMN charges_in_full;
ALTER TABLE credit_cards DROP CONSTRAINT credit_cards_grace_kind_check;
ALTER TABLE credit_cards ADD CONSTRAINT credit_cards_grace_kind_check
    CHECK (grace_kind IN ('statement', 'long'));
