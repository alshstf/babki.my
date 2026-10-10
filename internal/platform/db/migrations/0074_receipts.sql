-- +goose Up
-- Cash receipts (household stage 3, receipts plan): a receipt is the shop's
-- document of a purchase — named for good by its fiscal drive (fn) and the
-- document's number on it (fd), with the fiscal sign (fp), the till's time,
-- the total and which way the money went. It completes a row of the journal
-- (the card's spending the bank shows) rather than standing for a second
-- one; one the journal has no row for yet waits with no operation. The
-- seller, the address and the items come with the tax service's data, when
-- the family brings it (decisions Р-34, Р-36).
CREATE TABLE receipts (
    id           uuid PRIMARY KEY,
    space_id     uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    operation_id uuid REFERENCES operations(id) ON DELETE CASCADE,
    fn           text NOT NULL CHECK (fn ~ '^[0-9]{1,20}$'),
    fd           text NOT NULL CHECK (fd ~ '^[0-9]{1,20}$'),
    fp           text CHECK (fp ~ '^[0-9]{1,20}$'),
    kind         text NOT NULL CHECK (kind IN ('purchase', 'refund', 'payout', 'payout_refund')),
    issued_at    timestamp NOT NULL,
    total_minor  bigint NOT NULL CHECK (total_minor > 0),
    seller       text CHECK (char_length(seller) <= 300),
    seller_inn   text CHECK (seller_inn ~ '^[0-9]{10,12}$'),
    address      text CHECK (char_length(address) <= 500),
    items        jsonb NOT NULL DEFAULT '[]',
    source       text NOT NULL CHECK (source IN ('qr', 'fns', 'mail', 'file')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (space_id, fn, fd)
);
CREATE INDEX receipts_operation ON receipts (operation_id) WHERE operation_id IS NOT NULL;

-- The receipts written so far live in the rows' notes, as the receipt dialog
-- wrote them: «Чек 09.10.2026 19:15, ФН 7380440700000000, ФД 51243». The
-- first row of each receipt keeps it.
INSERT INTO receipts (id, space_id, operation_id, fn, fd, kind, issued_at, total_minor, source, created_at)
SELECT DISTINCT ON (o.space_id, m[6], m[7])
    gen_random_uuid(), o.space_id, o.id, m[6], m[7],
    CASE WHEN o.amount_minor > 0 THEN 'refund' ELSE 'purchase' END,
    -- Counted from the month's first day: a day past the month's end in an
    -- edited note runs on rather than failing the migration.
    make_timestamp(m[3]::int, m[2]::int, 1, m[4]::int, m[5]::int, 0) + (m[1]::int - 1) * interval '1 day',
    abs(o.amount_minor), 'qr', o.created_at
FROM operations o
CROSS JOIN LATERAL regexp_match(o.note,
    'Чек ([0-9]{2})\.([0-9]{2})\.([0-9]{4}) ([0-9]{2}):([0-9]{2}), ФН ([0-9]{1,20}), ФД ([0-9]{1,20})') AS m
WHERE m IS NOT NULL AND o.amount_minor <> 0 AND o.type IN ('withdrawal', 'deposit')
    AND m[1]::int BETWEEN 1 AND 31 AND m[2]::int BETWEEN 1 AND 12
    AND m[4]::int BETWEEN 0 AND 23 AND m[5]::int BETWEEN 0 AND 59
ORDER BY o.space_id, m[6], m[7], o.created_at;

-- +goose Down
DROP TABLE receipts;
