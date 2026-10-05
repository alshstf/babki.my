-- +goose Up
-- The day each trade's money settled, as T-Bank's broker report states it
-- (decision Р-3: a trade in another currency is priced at the official rate of
-- its settlement day). The operations the mirror holds do not carry that day;
-- they name their trades by exchange number (tradesInfo.trades[].num), and the
-- report names the same trades by the same number with the day beside it.
--
-- Kept beside the mirror rather than in it: a mirror row is what the
-- operations call answered and is never rewritten, while this is a second
-- source's answer about the same trades, arriving later and month by month.
CREATE TABLE tinvest_trade_settlements (
    link_id    UUID NOT NULL REFERENCES tinvest_account_links(id) ON DELETE CASCADE,
    trade_id   TEXT NOT NULL CHECK (trade_id <> ''),
    traded_at  TIMESTAMPTZ NOT NULL,
    settled_on DATE NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (link_id, trade_id)
);

-- Which months' reports have been read, and when. A month read after it was
-- long over is done for good; one read while its trades could still be
-- settling is read again later (see dueSettlementMonths).
CREATE TABLE tinvest_settlement_reports (
    link_id    UUID NOT NULL REFERENCES tinvest_account_links(id) ON DELETE CASCADE,
    month      DATE NOT NULL CHECK (month = date_trunc('month', month)::date),
    fetched_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (link_id, month)
);

-- +goose Down
DROP TABLE tinvest_settlement_reports;
DROP TABLE tinvest_trade_settlements;
