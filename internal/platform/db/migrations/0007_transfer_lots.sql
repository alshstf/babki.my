-- +goose Up
-- The FIFO lots an in-kind transfer moved, recorded next to the receiving
-- (transfer_in) operation. The single carried cost basis on that operation
-- says how much arrived but not when any of it was bought, so the whole
-- arrived position ends up dated on the transfer day — which misprices it in
-- the base currency, where every lot is converted at the rate of its own
-- purchase date. This is journal data, not metadata: it states what actually
-- moved.
--
-- Transfers recorded before this table are deliberately left without rows:
-- their source lots' dates are not recoverable, and inventing them would be
-- worse than an honest absence.
CREATE TABLE operation_transfer_lots (
    operation_id UUID NOT NULL REFERENCES operations(id) ON DELETE CASCADE,
    seq          INT NOT NULL,
    quantity     NUMERIC(30,10) NOT NULL CHECK (quantity > 0),
    cost_minor   BIGINT NOT NULL CHECK (cost_minor >= 0),
    acquired_on  DATE NOT NULL,
    PRIMARY KEY (operation_id, seq)
);

-- +goose Down
DROP TABLE operation_transfer_lots;
