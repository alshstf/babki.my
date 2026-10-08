package tinvest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/secretbox"
)

// The broker's dividend calendar, for the tax a foreign dividend lost abroad
// (Р-14). A foreign dividend arrives net of the issuer country's tax and the
// operation says only what arrived. The calendar has the declared amount per
// share and answers for any paper the broker knows, held here or not, so one
// connection's token serves its whole space. Once a day this job stores the
// calendar of every foreign paper the space has received a dividend on; the
// journal works the withheld tax out of it.

// DividendSource is what a stored calendar row says it came from.
const DividendSource = "tinvest"

// dividendHistoryBefore: the record date precedes the payment by weeks, more
// when the payment was delayed.
const dividendHistoryBefore = 365 * 24 * time.Hour

// dividendHorizon: a declared dividend is stored before it is paid.
const dividendHorizon = 365 * 24 * time.Hour

// brokerIDsPerPaper bounds the identifiers tried per paper: the import's,
// the catalog's figi and the ISIN search's listings.
const brokerIDsPerPaper = 4

// RefreshDividendsArgs is the daily job that reads the dividend calendar of
// the foreign papers each connected space has received dividends on.
type RefreshDividendsArgs struct{}

func (RefreshDividendsArgs) Kind() string { return "tinvest.refresh_dividends" }

// dividendStore is the narrow view of marketdata.Store this job writes to.
type dividendStore interface {
	ReplaceDividends(ctx context.Context, instrumentID uuid.UUID, source string,
		dividends []marketdata.Dividend, fetchedAt time.Time) error
}

type dividendsWorker struct {
	river.WorkerDefaults[RefreshDividendsArgs]
	store     *Store
	dividends dividendStore
	box       *secretbox.Box
	newClient clientFactory
	log       *slog.Logger
	now       func() time.Time
}

// NewDividendsWorker builds the River worker that stores the broker's dividend
// calendar. now is the clock the horizon is measured from; nil for time.Now.
func NewDividendsWorker(store *Store, dividends dividendStore, box *secretbox.Box, newClient clientFactory,
	log *slog.Logger, now func() time.Time,
) river.Worker[RefreshDividendsArgs] {
	if now == nil {
		now = time.Now
	}
	return &dividendsWorker{store: store, dividends: dividends, box: box, newClient: newClient, log: log, now: now}
}

func (w *dividendsWorker) Timeout(*river.Job[RefreshDividendsArgs]) time.Duration {
	return 5 * time.Minute
}

// Work reads the calendar for each active connection's space. One
// connection's failure does not stop the others (as in quotesWorker.Work), and a
// paper served once is not asked again: the calendar is the broker's.
func (w *dividendsWorker) Work(ctx context.Context, _ *river.Job[RefreshDividendsArgs]) error {
	conns, err := w.store.ListActiveConnections(ctx)
	if err != nil {
		return fmt.Errorf("tinvest: list active connections: %w", err)
	}
	served := map[uuid.UUID]bool{}
	var lastErr error
	stored := 0
	for _, conn := range conns {
		n, err := w.fillConnection(ctx, conn, served)
		stored += n
		if err == nil {
			continue
		}
		if errors.Is(err, ErrTokenInvalid) {
			w.log.Warn("tinvest: the broker rejected this connection's token while reading dividends",
				"connection_id", conn.ID)
			if err := w.store.UpdateConnectionStatus(ctx, conn.ID, StatusTokenRevoked); err != nil {
				w.log.Error("tinvest: recording a revoked token failed", "connection_id", conn.ID, "err", err)
			}
			continue
		}
		lastErr = err
		w.log.Error("tinvest: reading a space's dividend calendar failed", "connection_id", conn.ID, "err", err)
	}
	w.log.Info("tinvest: dividend calendars refreshed", "connections", len(conns), "papers", stored)
	return lastErr
}

// fillConnection stores the calendar of every foreign paper the connection's
// space has received dividends on and no earlier connection of this run has
// served. It returns how many papers it stored.
func (w *dividendsWorker) fillConnection(ctx context.Context, conn Connection, served map[uuid.UUID]bool) (int, error) {
	papers, err := w.store.ForeignDividendPapers(ctx, conn.SpaceID, conn.ID)
	if err != nil {
		return 0, err
	}
	var todo []DividendPaper
	for _, p := range papers {
		if !served[p.InstrumentID] {
			todo = append(todo, p)
		}
	}
	if len(todo) == 0 {
		return 0, nil
	}
	token, err := w.box.Open(conn.TokenCiphertext)
	if err != nil {
		return 0, fmt.Errorf("tinvest: open token of connection %s: %w", conn.ID, err)
	}
	client, err := w.newClient(string(token))
	if err != nil {
		return 0, err
	}

	now := w.now()
	stored := 0
	for _, p := range todo {
		declared, ok, err := w.calendarOf(ctx, client, p, now)
		if err != nil {
			return stored, err
		}
		if !ok {
			continue
		}
		rows := make([]marketdata.Dividend, 0, len(declared))
		for _, d := range declared {
			rows = append(rows, marketdata.Dividend{
				InstrumentID: p.InstrumentID,
				Source:       DividendSource,
				RecordDate:   d.RecordDate,
				PaymentDate:  d.PaymentDate,
				LastBuyDate:  d.LastBuyDate,
				PerShare:     d.PerShare.Decimal(),
				Currency:     d.PerShare.Currency,
			})
		}
		if err := w.dividends.ReplaceDividends(ctx, p.InstrumentID, DividendSource, rows, now); err != nil {
			return stored, err
		}
		served[p.InstrumentID] = true
		stored++
	}
	return stored, nil
}

// calendarOf asks for a paper's dividends under each identifier until one
// answers with any. ok is false, and the stored calendar kept, when none does:
// the space has received dividends on it, so empty means a wrong identifier.
func (w *dividendsWorker) calendarOf(ctx context.Context, client *Client, p DividendPaper, now time.Time,
) ([]DeclaredDividend, bool, error) {
	from := p.FirstDividendOn.Add(-dividendHistoryBefore)
	to := now.Add(dividendHorizon)
	for _, id := range brokerIDsOf(ctx, client, w.log, p) {
		declared, err := client.Dividends(ctx, id, from, to)
		switch {
		case errors.Is(err, ErrTokenInvalid):
			return nil, false, err
		case err != nil:
			w.log.Debug("tinvest: the broker's dividend calendar did not answer for this identifier",
				"instrument_id", p.InstrumentID, "broker_id", id, "err", err)
			continue
		case len(declared) == 0:
			continue
		}
		return declared, true, nil
	}
	w.log.Info("tinvest: no dividend calendar found for a paper the space has received dividends on",
		"instrument_id", p.InstrumentID, "isin", p.ISIN)
	return nil, false, nil
}

// brokerIDsOf is every identifier the broker may know a paper by, best first:
// the import's listing, the catalog's figi, the ISIN search's listings. A failed
// search leaves the first two.
func brokerIDsOf(ctx context.Context, client *Client, log *slog.Logger, p DividendPaper) []string {
	var ids []string
	add := func(id string) {
		if id != "" && !slices.Contains(ids, id) && len(ids) < brokerIDsPerPaper {
			ids = append(ids, id)
		}
	}
	add(p.InstrumentUID)
	add(p.FIGI)
	if len(ids) >= brokerIDsPerPaper || p.ISIN == "" {
		return ids
	}
	found, err := client.FindInstruments(ctx, p.ISIN)
	if err != nil {
		log.Debug("tinvest: searching the broker for a paper's listings failed",
			"instrument_id", p.InstrumentID, "isin", p.ISIN, "err", err)
		return ids
	}
	for _, l := range found {
		if strings.EqualFold(l.ISIN, p.ISIN) {
			add(l.UID)
		}
	}
	return ids
}

// DividendPaper is a foreign paper a space has received dividends on, with
// what the broker may know it by.
type DividendPaper struct {
	InstrumentID    uuid.UUID
	ISIN, FIGI      string
	InstrumentUID   string
	FirstDividendOn time.Time
}

// ForeignDividendPapers lists non-Russian-ISIN papers any account of the space
// has a dividend on, with this connection's mapped listing and the first dividend
// day. Foreign by ISIN country: a Russian issuer's tax comes as its own broker
// line; a foreign one is taken abroad, out of sight.
func (s *Store) ForeignDividendPapers(ctx context.Context, spaceID, connID uuid.UUID) ([]DividendPaper, error) {
	const what = "list the foreign papers with dividends"
	firsts, err := s.journal().FirstDaysInSpace(ctx, spaceID, operation.TypeDividend)
	if err != nil {
		return nil, fmt.Errorf("tinvest: %s: read the journal: %w", what, err)
	}
	ids := make([]uuid.UUID, 0, len(firsts))
	for id := range firsts {
		ids = append(ids, id)
	}
	papers, err := s.catalogOf(ctx, what, ids)
	if err != nil {
		return nil, err
	}
	var foreign []uuid.UUID
	for _, id := range byID(ids) {
		if isin := papers[id].ISIN; isin != "" && !strings.HasPrefix(strings.ToUpper(isin), "RU") {
			foreign = append(foreign, id)
		}
	}
	// The most recently updated listing of each, as the resolver's figi lookup
	// picks.
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT ON (instrument_id) instrument_id, instrument_uid
		FROM tinvest_instrument_map
		WHERE connection_id = $1 AND instrument_id = ANY($2)
		ORDER BY instrument_id, updated_at DESC`, connID, foreign)
	if err != nil {
		return nil, fmt.Errorf("tinvest: %s: %w", what, err)
	}
	uids := map[uuid.UUID]string{}
	for rows.Next() {
		var (
			id  uuid.UUID
			uid string
		)
		if err := rows.Scan(&id, &uid); err != nil {
			rows.Close()
			return nil, fmt.Errorf("tinvest: %s: %w", what, err)
		}
		uids[id] = uid
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tinvest: %s: %w", what, err)
	}
	var out []DividendPaper
	for _, id := range foreign {
		out = append(out, DividendPaper{
			InstrumentID: id, ISIN: papers[id].ISIN, FIGI: papers[id].FIGI,
			InstrumentUID: uids[id], FirstDividendOn: firsts[id],
		})
	}
	return out, nil
}
