package marketdata

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
)

// BondEventKind is what a bond's schedule says happens on a day.
type BondEventKind string

const (
	// BondCoupon is interest paid on the face outstanding.
	BondCoupon BondEventKind = "coupon"
	// BondAmortization is a part of the face repaid before maturity.
	BondAmortization BondEventKind = "amortization"
	// BondRedemption is the face repaid at maturity.
	BondRedemption BondEventKind = "redemption"
	// BondOffer is a day the issuer offers to buy the bond back; holding on is
	// the holder's choice, so it pays nothing by itself.
	BondOffer BondEventKind = "offer"
)

// BondEvent is one day of a bond's schedule, per unit of the bond. Value is
// in Currency, the face's; nil when not yet known — a floating coupon before
// its rate is set. Percent is the coupon's annual rate, or the repaid share of
// the face; nil when the source gives none.
type BondEvent struct {
	InstrumentID uuid.UUID
	Source       string
	Kind         BondEventKind
	On           time.Time
	RecordOn     *time.Time
	Value        *decimal.Decimal
	Percent      *decimal.Decimal
	Currency     string
}

// ReplaceBondEvents stores a source's whole schedule of one bond in place of
// the one it gave before.
func (s *Store) ReplaceBondEvents(ctx context.Context, instrumentID uuid.UUID, source string,
	events []BondEvent, fetchedAt time.Time,
) error {
	batch := &pgx.Batch{}
	batch.Queue(`DELETE FROM bond_events WHERE instrument_id = $1 AND source = $2`, instrumentID, source)
	for _, e := range events {
		if e.InstrumentID != instrumentID || e.Source != source {
			return fmt.Errorf("marketdata: a bond event of %s from %q handed in with the schedule of %s from %q",
				e.InstrumentID, e.Source, instrumentID, source)
		}
		batch.Queue(`
			INSERT INTO bond_events (instrument_id, source, kind, on_date, record_date, value, percent, currency, fetched_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (instrument_id, source, kind, on_date) DO NOTHING`,
			e.InstrumentID, e.Source, e.Kind, e.On, e.RecordOn, e.Value, e.Percent, e.Currency, fetchedAt)
	}
	return runBatch(ctx, s.db, batch, 1+len(events))
}

// BondEventsBetween is the schedule of the given bonds from from to to, both
// days included, by bond, earliest first.
func (s *Store) BondEventsBetween(ctx context.Context, instrumentIDs []uuid.UUID, from, to time.Time) (map[uuid.UUID][]BondEvent, error) {
	out := map[uuid.UUID][]BondEvent{}
	if len(instrumentIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT instrument_id, source, kind, on_date, record_date, value, percent, currency
		FROM bond_events
		WHERE instrument_id = ANY($1) AND on_date BETWEEN $2 AND $3
		ORDER BY instrument_id, on_date, kind`, instrumentIDs, from, to)
	if err != nil {
		return nil, fmt.Errorf("marketdata: bond events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e BondEvent
		if err := rows.Scan(&e.InstrumentID, &e.Source, &e.Kind, &e.On, &e.RecordOn, &e.Value, &e.Percent, &e.Currency); err != nil {
			return nil, fmt.Errorf("marketdata: bond events: %w", err)
		}
		out[e.InstrumentID] = append(out[e.InstrumentID], e)
	}
	return out, rows.Err()
}

// RefreshBondSchedulesArgs is the daily job that stores the exchange's
// schedule of every bond the journals hold, for the payouts calendar (#399).
type RefreshBondSchedulesArgs struct{}

func (RefreshBondSchedulesArgs) Kind() string { return "marketdata.refresh_bond_schedules" }

// BondScheduleFeed publishes bonds' schedules.
type BondScheduleFeed interface {
	// BondSchedule is the bond's whole schedule by its ISIN or, without one, its
	// ticker; found is false when the source does not know the bond.
	BondSchedule(ctx context.Context, isin, ticker string) (events []BondEvent, found bool, err error)
	Name() string
}

type bondScheduleWorker struct {
	river.WorkerDefaults[RefreshBondSchedulesArgs]
	store       *Store
	ops         instrumentFirstDays
	instruments instrumentsByID
	feed        BondScheduleFeed
	log         *slog.Logger
	now         func() time.Time
}

// NewBondScheduleWorker builds the River worker that stores the feed's bond
// schedules.
func NewBondScheduleWorker(store *Store, ops instrumentFirstDays, instruments instrumentsByID,
	feed BondScheduleFeed, log *slog.Logger,
) river.Worker[RefreshBondSchedulesArgs] {
	return &bondScheduleWorker{store: store, ops: ops, instruments: instruments, feed: feed, log: log, now: time.Now}
}

func (w *bondScheduleWorker) Timeout(*river.Job[RefreshBondSchedulesArgs]) time.Duration {
	return backfillTimeout
}

// Work stores the schedule of every bond the journals name. One bond's
// failure does not stop the others; the last error is returned at the end so
// River retries.
func (w *bondScheduleWorker) Work(ctx context.Context, _ *river.Job[RefreshBondSchedulesArgs]) error {
	first, err := w.ops.FirstDaysByInstrument(ctx)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, 0, len(first))
	for id := range first {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	papers, err := w.instruments.ByIDs(ctx, ids)
	if err != nil {
		return err
	}
	now := w.now()
	var lastErr error
	for _, id := range ids {
		paper, ok := papers[id]
		if !ok || paper.Type != instrument.TypeBond {
			continue
		}
		events, found, err := w.feed.BondSchedule(ctx, paper.ISIN, paper.Ticker)
		if err != nil {
			w.log.Warn("marketdata: fetch a bond's schedule failed", "instrument", id, "source", w.feed.Name(), "err", err)
			lastErr = err
			continue
		}
		if !found {
			w.log.Debug("marketdata: no schedule for a bond", "instrument", id, "isin", paper.ISIN)
			continue
		}
		for i := range events {
			events[i].InstrumentID, events[i].Source = id, w.feed.Name()
		}
		if err := w.store.ReplaceBondEvents(ctx, id, w.feed.Name(), events, now); err != nil {
			return err
		}
		w.log.Info("marketdata: stored a bond's schedule", "instrument", id, "source", w.feed.Name(), "events", len(events))
	}
	return lastErr
}
