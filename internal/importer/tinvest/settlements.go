package tinvest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/operation"
)

// The day a trade's money settled (decision Р-3).
//
// A trade in another currency is priced at the official rate of the day its
// money actually moved — the settlement day, a day or two after the trade on
// an exchange — and at the trade's own day only when the settlement day is not
// known. The operations call does not say it. The broker report does, per
// trade, and names each trade by the same exchange number the operation lists
// under tradesInfo; what is here reads the report month by month, keeps its
// answers beside the mirror, and lays them onto the trades on every rebuild.
//
// A MISSING DAY IS NEVER AN ERROR. The report is slow to build and limited to
// a few calls a minute, and a month the broker cannot hand over this hour
// leaves its trades priced on their trade day — what every trade was priced on
// before this existed — until a later run reads it.

// settlementGrace is how long after a month ends its report is taken as final:
// a month read later than that is not read again. Settlement takes days, so
// ten days past the month's end every trade in it has its day.
const settlementGrace = 10 * 24 * time.Hour

// settlementRecheck is how soon a month read before it was final may be read
// again. Once a day is enough for trades that settle in a day or two, and it
// keeps the hourly run from spending its few report calls on the same month.
const settlementRecheck = 20 * time.Hour

// settlementBudget is how long one run goes on ordering new months' reports.
// It is checked between months, so one report may run past it by its own
// wait (see brokerReportMaxPolls). A long history is read over several hourly
// runs, newest months first.
const settlementBudget = 3 * time.Minute

// maxImportedSettlementLag is the furthest after its trade a settlement day
// read from the report is believed. Exchange trades settle in days; a day
// further off than a month says the numbers were matched to the wrong trade,
// and the trade keeps its own day instead.
const maxImportedSettlementLag = 31 * 24 * time.Hour

// tradeSettlementSource is the broker report as this file uses it. *Client
// satisfies it.
type tradeSettlementSource interface {
	TradeSettlements(ctx context.Context, brokerAccountID string, from, to time.Time) ([]TradeSettlement, error)
}

// readSettlements reads the reports of the months whose trades have no
// settlement day yet, for every link, until the budget is spent.
//
// IT FAILS NOTHING. A report the broker will not hand over is logged and ends
// this run's reading — the next month would meet the same limit or the same
// fault — and the sync goes on to rebuild with whatever days it has.
func readSettlements(ctx context.Context, store *Store, src tradeSettlementSource, links []AccountLink,
	now func() time.Time, budget time.Duration, log *slog.Logger,
) {
	deadline := now().Add(budget)
	for _, link := range links {
		months, err := store.dueSettlementMonthsOf(ctx, link.ID, now())
		if err != nil {
			logAt(ctx, log, err, "tinvest: list the months whose settlement days are missing failed",
				"link", link.ID, "err", err)
			return
		}
		for _, month := range months {
			if !now().Before(deadline) {
				return
			}
			trades, err := src.TradeSettlements(ctx, link.BrokerAccountID, month, month.AddDate(0, 1, 0))
			if err != nil {
				logAt(ctx, log, err, "tinvest: read the broker report's settlement days failed, trades keep their trade day",
					"link", link.ID, "month", month.Format("2006-01"), "err", err)
				return
			}
			if err := store.saveTradeSettlements(ctx, link.ID, month, trades, now()); err != nil {
				logAt(ctx, log, err, "tinvest: store the broker report's settlement days failed",
					"link", link.ID, "month", month.Format("2006-01"), "err", err)
				return
			}
			log.Info("tinvest: read settlement days from the broker report",
				"link", link.ID, "month", month.Format("2006-01"), "trades", len(trades))
		}
	}
}

// dueSettlementMonths is which months' reports to read now, newest first: those
// with trades still missing their day that were never read, or were read
// before they were final and not within the last settlementRecheck.
func dueSettlementMonths(unsettled []time.Time, read map[time.Time]time.Time, now time.Time) []time.Time {
	var due []time.Time
	for _, month := range unsettled {
		at, ok := read[month]
		if ok && !at.Before(month.AddDate(0, 1, 0).Add(settlementGrace)) {
			continue
		}
		if ok && now.Sub(at) < settlementRecheck {
			continue
		}
		due = append(due, month)
	}
	slices.SortFunc(due, func(a, b time.Time) int { return b.Compare(a) })
	return due
}

// monthOf is the first day of t's month, in UTC — the calendar the report's
// period is asked in.
func monthOf(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// wireTrades is the part of a mirror row's raw operation that lists its trades.
type wireTrades struct {
	TradesInfo struct {
		Trades []struct {
			Num  string `json:"num"`
			Date string `json:"date"`
		} `json:"trades"`
	} `json:"tradesInfo"`
}

// settlementDay is the day a row's trades settled: the one day every trade it
// lists has, or nil when the row lists none, any of them is not known yet, or
// they disagree. Partial knowledge is no knowledge — the row is one journal
// entry and has one day.
func settlementDay(raw json.RawMessage, known map[string]time.Time) *time.Time {
	if len(known) == 0 {
		return nil
	}
	var w wireTrades
	if json.Unmarshal(raw, &w) != nil || len(w.TradesInfo.Trades) == 0 {
		return nil
	}
	var day time.Time
	for i, t := range w.TradesInfo.Trades {
		d, ok := known[t.Num]
		if !ok || (i > 0 && !d.Equal(day)) {
			return nil
		}
		day = d
	}
	return &day
}

// settleOn writes day onto a trade entry as the day its money settled. Only
// the entries that ARE the trade take it — a purchase, a sale, a currency
// exchange — and not a commission charged beside one, which is paid when it is
// charged. A day before the entry's own, or implausibly long after it, is not
// believed (see maxImportedSettlementLag).
func settleOn(op *operation.Operation, day *time.Time) {
	if day == nil {
		return
	}
	switch op.Type {
	case operation.TypeBuy, operation.TypeSell, operation.TypeConversion:
	default:
		return
	}
	if day.Before(op.OccurredOn) || day.Sub(op.OccurredOn) > maxImportedSettlementLag {
		return
	}
	d := *day
	op.SettledOn = &d
}

// dueSettlementMonthsOf is dueSettlementMonths over what the store holds for
// one link.
func (s *Store) dueSettlementMonthsOf(ctx context.Context, linkID uuid.UUID, now time.Time) ([]time.Time, error) {
	unsettled, err := s.unsettledTradeMonths(ctx, linkID)
	if err != nil {
		return nil, err
	}
	if len(unsettled) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(ctx,
		`SELECT month, fetched_at FROM tinvest_settlement_reports WHERE link_id = $1`, linkID)
	if err != nil {
		return nil, fmt.Errorf("tinvest: list the settlement reports read: %w", err)
	}
	defer rows.Close()
	read := map[time.Time]time.Time{}
	for rows.Next() {
		var month, at time.Time
		if err := rows.Scan(&month, &at); err != nil {
			return nil, fmt.Errorf("tinvest: list the settlement reports read: %w", err)
		}
		read[monthOf(month)] = at
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tinvest: list the settlement reports read: %w", err)
	}
	return dueSettlementMonths(unsettled, read, now), nil
}

// unsettledTradeMonths is the months of the trades a link's mirror lists that
// have no settlement day stored, each once.
//
// The month is the TRADE's, read here rather than in SQL: a date the database
// failed to cast would fail the whole query, and one unreadable trade would
// then stop every month from being read. A trade whose own date does not parse
// takes its operation's.
func (s *Store) unsettledTradeMonths(ctx context.Context, linkID uuid.UUID) ([]time.Time, error) {
	rows, err := s.db.Query(ctx, `
		SELECT COALESCE(t->>'date', ''), m.occurred_at
		FROM tinvest_operations_mirror m
		CROSS JOIN LATERAL jsonb_array_elements(
			CASE WHEN jsonb_typeof(m.raw->'tradesInfo'->'trades') = 'array'
			     THEN m.raw->'tradesInfo'->'trades' ELSE '[]'::jsonb END) AS t
		LEFT JOIN tinvest_trade_settlements s
		       ON s.link_id = m.link_id AND s.trade_id = t->>'num'
		WHERE m.link_id = $1 AND m.disappeared_at IS NULL
		  AND COALESCE(t->>'num', '') <> '' AND s.trade_id IS NULL`, linkID)
	if err != nil {
		return nil, fmt.Errorf("tinvest: list the trades with no settlement day: %w", err)
	}
	defer rows.Close()
	seen := map[time.Time]bool{}
	var months []time.Time
	for rows.Next() {
		var date string
		var occurred time.Time
		if err := rows.Scan(&date, &occurred); err != nil {
			return nil, fmt.Errorf("tinvest: list the trades with no settlement day: %w", err)
		}
		at, err := parseWireTime(date)
		if err != nil || at.IsZero() {
			at = occurred
		}
		if month := monthOf(at); !seen[month] {
			seen[month] = true
			months = append(months, month)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tinvest: list the trades with no settlement day: %w", err)
	}
	return months, nil
}

// saveTradeSettlements stores one month's report and that it was read, as one
// write. A trade the report states again takes the day stated last.
func (s *Store) saveTradeSettlements(ctx context.Context, linkID uuid.UUID, month time.Time,
	trades []TradeSettlement, now time.Time,
) error {
	// One row per trade: an upsert touching one key twice is refused whole.
	byID := make(map[string]TradeSettlement, len(trades))
	ids := make([]string, 0, len(trades))
	for _, t := range trades {
		if _, dup := byID[t.TradeID]; !dup {
			ids = append(ids, t.TradeID)
		}
		byID[t.TradeID] = t
	}
	traded := make([]time.Time, len(ids))
	settled := make([]time.Time, len(ids))
	for i, id := range ids {
		traded[i] = byID[id].TradedAt
		settled[i] = byID[id].SettledOn
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("tinvest: store settlement days: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if len(ids) > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO tinvest_trade_settlements (link_id, trade_id, traded_at, settled_on)
			SELECT $1, u.trade_id, u.traded_at, u.settled_on
			FROM unnest($2::text[], $3::timestamptz[], $4::date[]) AS u(trade_id, traded_at, settled_on)
			ON CONFLICT (link_id, trade_id) DO UPDATE
			SET traded_at = EXCLUDED.traded_at, settled_on = EXCLUDED.settled_on, updated_at = now()
			WHERE (tinvest_trade_settlements.traded_at, tinvest_trade_settlements.settled_on)
			      IS DISTINCT FROM (EXCLUDED.traded_at, EXCLUDED.settled_on)`,
			linkID, ids, traded, settled); err != nil {
			return fmt.Errorf("tinvest: store settlement days: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO tinvest_settlement_reports (link_id, month, fetched_at) VALUES ($1, $2, $3)
		ON CONFLICT (link_id, month) DO UPDATE SET fetched_at = EXCLUDED.fetched_at`,
		linkID, monthOf(month), now); err != nil {
		return fmt.Errorf("tinvest: record the settlement report read: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tinvest: store settlement days: %w", err)
	}
	return nil
}

// tradeSettlementsByLink is every settlement day stored for a link, by trade
// number.
func (s *Store) tradeSettlementsByLink(ctx context.Context, linkID uuid.UUID) (map[string]time.Time, error) {
	rows, err := s.db.Query(ctx,
		`SELECT trade_id, settled_on FROM tinvest_trade_settlements WHERE link_id = $1`, linkID)
	if err != nil {
		return nil, fmt.Errorf("tinvest: list the settlement days of a link: %w", err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id string
		var day time.Time
		if err := rows.Scan(&id, &day); err != nil {
			return nil, fmt.Errorf("tinvest: list the settlement days of a link: %w", err)
		}
		out[id] = day
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tinvest: list the settlement days of a link: %w", err)
	}
	return out, nil
}
