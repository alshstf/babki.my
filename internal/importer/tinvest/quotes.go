package tinvest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/secretbox"
)

// A CONNECTED BROKER IS A QUOTE SOURCE, AND FOR THIS PORTFOLIO THE BEST ONE.
// It needs no second key, no signup and no separate terms — the token is
// already there and the data is the owner's own broker's — and it covers three
// things the exchange's public feed does not:
//
//   - foreign shares. The owner holds eight, and MOEX has never quoted them;
//     the broker's price for Apple on 2026-08-08 was 313,25 $, against
//     313,33 $ on the Nasdaq the same evening.
//   - a delisted fund with a dealer's price. The FinEx funds stopped trading in
//     2023 and no exchange anywhere prices them, yet the broker quotes them
//     over the counter — which is the only price at which those units could
//     actually be sold, and the eight of them are a real part of this portfolio.
//   - papers that stopped trading. Their price comes back stamped with the day
//     it was last struck — Tesla's old ruble line still answers with
//     2022-02-25 — so a stale price says so by its date, in the field the
//     screen already shows, with nothing extra to invent.
//
// WHAT IT DOES NOT COVER is anything the broker does not list, which for this
// owner is whatever sits only at a second broker's exchange. The public MOEX
// feed stays exactly as it is: the two write into the same table and neither is
// aware of the other, and where both price the same paper on the same day the
// later run wins. That is deliberate. They are two observations of ONE price
// rather than two derivations of one figure, they differ by a few hundredths of
// a percent, and every row carries the source it came from.

// lastPricesBatch is how many instruments one GetLastPrices asks about.
//
// The method's own documentation states no ceiling, and none was found by
// experiment either (a request is not something to probe for its breaking point
// against a live broker). 100 is chosen to be plainly under any plausible one
// while keeping a portfolio of this size to a single request — the whole point
// of the batch, since the limit that IS documented is on requests per minute.
const lastPricesBatch = 100

// LastPrice is one instrument's most recent price as the broker reports it.
//
// THE INSTANT IS THE BROKER'S, never this program's clock: it is when the price
// was struck, and for a paper that stopped trading it is years ago. That is the
// whole of how a stale price announces itself, so nothing here substitutes a
// fresher-looking day for it (the fault #90 was about, in the exchange feed).
//
// Dealer says the price came from the broker acting as a dealer rather than
// from an exchange. It is not a lesser price — for a delisted fund it is the
// only one there is, and the one the units could be sold at — but it is a
// different fact, and the row that stores it says which.
type LastPrice struct {
	InstrumentUID string
	Price         decimal.Decimal
	At            time.Time
	Dealer        bool
}

// LastPrices asks the broker for the latest price of each instrument.
//
// An instrument the broker has no price for comes back WITHOUT a price field
// and is left out of the result entirely rather than returned as a zero — the
// difference between "no price" and "a price of nothing" is exactly what a
// valuation must not lose. The broker really does answer that way: on the
// owner's own catalog one of the FinEx identifiers returns an entry with no
// price, no figi and no ticker at all.
func (c *Client) LastPrices(ctx context.Context, instrumentUIDs []string) ([]LastPrice, error) {
	out := make([]LastPrice, 0, len(instrumentUIDs))
	for start := 0; start < len(instrumentUIDs); start += lastPricesBatch {
		end := min(start+lastPricesBatch, len(instrumentUIDs))
		var resp wireGetLastPricesResponse
		req := struct {
			InstrumentID []string `json:"instrumentId"`
		}{InstrumentID: instrumentUIDs[start:end]}
		if err := c.do(ctx, "MarketDataService/GetLastPrices", req, &resp); err != nil {
			return nil, err
		}
		for _, w := range resp.LastPrices {
			p, ok, err := w.parse()
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// RefreshQuotesArgs is the periodic job that prices every mapped instrument of
// every active connection.
type RefreshQuotesArgs struct{}

func (RefreshQuotesArgs) Kind() string { return "tinvest.refresh_quotes" }

// quoteStore is the narrow view of marketdata.Store this worker needs.
type quoteStore interface {
	UpsertQuotes(ctx context.Context, quotes []marketdata.Quote) error
}

type quotesWorker struct {
	river.WorkerDefaults[RefreshQuotesArgs]
	store     *Store
	quotes    quoteStore
	box       *secretbox.Box
	newClient clientFactory
	log       *slog.Logger
	now       func() time.Time
}

// NewQuotesWorker builds the River worker that stores broker prices. now is the
// clock the "is this price dated in the future" guard reads; pass nil for
// time.Now.
func NewQuotesWorker(store *Store, quotes quoteStore, box *secretbox.Box, newClient clientFactory, log *slog.Logger, now func() time.Time) river.Worker[RefreshQuotesArgs] {
	if now == nil {
		now = time.Now
	}
	return &quotesWorker{store: store, quotes: quotes, box: box, newClient: newClient, log: log, now: now}
}

func (w *quotesWorker) Timeout(*river.Job[RefreshQuotesArgs]) time.Duration {
	return 5 * time.Minute
}

// Work prices what every active connection has mapped.
//
// ONE CONNECTION'S FAILURE DOES NOT STOP THE OTHERS. A revoked token, a broker
// having a bad minute — each is that connection's problem, and returning it
// would leave every other space unpriced until it was fixed. The last error
// seen is returned once at the end so River still retries, after everything
// that could be priced has been.
//
// A revoked token is NOT returned at all and marks the connection instead, the
// same way the sync worker treats it: retrying cannot un-revoke a token, and
// the owner is told through the connection's own status.
func (w *quotesWorker) Work(ctx context.Context, _ *river.Job[RefreshQuotesArgs]) error {
	conns, err := w.store.ListActiveConnections(ctx)
	if err != nil {
		w.log.Error("tinvest: list active connections failed", "err", err)
		return err
	}
	if len(conns) == 0 {
		w.log.Debug("tinvest: no active connections, nothing to price")
		return nil
	}

	var lastErr error
	stored := 0
	for _, conn := range conns {
		n, err := w.priceConnection(ctx, conn)
		stored += n
		if err != nil {
			if errors.Is(err, ErrTokenInvalid) {
				w.markRevoked(ctx, conn)
				continue
			}
			lastErr = err
			w.log.Error("tinvest: pricing a connection's instruments failed",
				"connection_id", conn.ID, "err", err)
		}
	}
	w.log.Info("tinvest: quotes refreshed", "connections", len(conns), "quotes", stored)
	return lastErr
}

// markRevoked records that the broker no longer accepts this token. The write
// failing is logged and not returned: the caller is already past the point of
// doing anything about this connection, and one connection's bookkeeping must
// not stop the others being priced.
func (w *quotesWorker) markRevoked(ctx context.Context, conn Connection) {
	w.log.Warn("tinvest: the broker rejected this connection's token while fetching prices",
		"connection_id", conn.ID)
	if err := w.store.UpdateConnectionStatus(ctx, conn.ID, StatusTokenRevoked); err != nil {
		w.log.Error("tinvest: recording a revoked token failed", "connection_id", conn.ID, "err", err)
	}
}

func (w *quotesWorker) priceConnection(ctx context.Context, conn Connection) (int, error) {
	listings, err := w.store.QuotableByConnection(ctx, conn.ID)
	if err != nil {
		return 0, err
	}
	if len(listings) == 0 {
		w.log.Debug("tinvest: connection has no mapped instruments to price", "connection_id", conn.ID)
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

	// The currency of a listing recorded before it was kept (migration 0017),
	// learned once and remembered. Done BEFORE the prices are asked for, so a
	// listing is either priced with its own currency or not priced at all —
	// never priced under the catalog row's, which belongs to another venue.
	listings = w.fillCurrencies(ctx, conn, client, listings)

	byUID := make(map[string]QuotableInstrument, len(listings))
	uids := make([]string, 0, len(listings))
	for _, l := range listings {
		if l.Currency == "" {
			continue
		}
		byUID[l.InstrumentUID] = l
		uids = append(uids, l.InstrumentUID)
	}
	if len(uids) == 0 {
		return 0, nil
	}

	prices, err := client.LastPrices(ctx, uids)
	if err != nil {
		return 0, err
	}

	today := mskDay(w.now())
	quotes := make([]marketdata.Quote, 0, len(prices))
	for _, p := range prices {
		listing, ok := byUID[p.InstrumentUID]
		if !ok {
			// A price for something this run did not ask about. Nothing to
			// store it against, and no reason to fail over it.
			w.log.Debug("tinvest: a price arrived for an instrument this run did not ask about",
				"instrument_uid", p.InstrumentUID)
			continue
		}
		if !p.Price.IsPositive() {
			w.log.Debug("tinvest: the broker reports a non-positive price, which is no price",
				"instrument_uid", p.InstrumentUID, "price", p.Price.String())
			continue
		}
		on := mskDay(p.At)
		if on.After(today) {
			// The same guard the exchange feed keeps, and for the same reason:
			// the latest quote is chosen by ORDER BY on_date DESC, so a single
			// row dated in the future outranks every genuine refresh after it
			// for as long as that date is in the future — silently, on every
			// position the instrument appears in.
			w.log.Warn("tinvest: refusing a price dated in the future",
				"instrument_uid", p.InstrumentUID, "on", on.Format(time.DateOnly))
			continue
		}
		quotes = append(quotes, marketdata.Quote{
			InstrumentID: listing.InstrumentID,
			On:           on,
			Price:        p.Price,
			Currency:     listing.Currency,
			Source:       quoteSource(p.Dealer),
		})
	}
	if len(quotes) == 0 {
		return 0, nil
	}
	if err := w.quotes.UpsertQuotes(ctx, quotes); err != nil {
		return 0, err
	}
	return len(quotes), nil
}

// SourceExchange and SourceDealer are what a stored quote's source says about
// where the broker's price came from. They are two values rather than one
// because they are two different facts about the same number: an exchange
// struck the first, and the broker itself stands behind the second — which for
// a delisted fund is the only price there is, and is still not a market price.
const (
	SourceExchange = "tinvest"
	SourceDealer   = "tinvest_dealer"
)

func quoteSource(dealer bool) string {
	if dealer {
		return SourceDealer
	}
	return SourceExchange
}

// fillCurrencies learns the currency of every listing that has none, one
// passport request each, and remembers it.
//
// A failure here costs that listing its price for this run and nothing more:
// the listing is returned with an empty currency and the caller leaves it out.
// Nothing is guessed — pricing it under the catalog row's currency is the one
// answer that would look right and be wrong.
func (w *quotesWorker) fillCurrencies(ctx context.Context, conn Connection, src passportSource, listings []QuotableInstrument) []QuotableInstrument {
	for i, l := range listings {
		if l.Currency != "" {
			continue
		}
		brief, err := src.InstrumentByUID(ctx, l.InstrumentUID)
		if err != nil {
			if errors.Is(err, ErrTokenInvalid) {
				// Nothing further will work either; leave the rest empty and
				// let the caller's own request surface it.
				return listings
			}
			w.log.Debug("tinvest: could not learn what a listing is denominated in, leaving it unpriced",
				"instrument_uid", l.InstrumentUID, "err", err)
			continue
		}
		currency := upperCurrency(brief.Currency)
		if currency == "" {
			w.log.Debug("tinvest: the broker's passport names no currency for this listing",
				"instrument_uid", l.InstrumentUID)
			continue
		}
		if err := w.store.SetMapCurrency(ctx, conn.ID, l.InstrumentUID, currency); err != nil {
			w.log.Error("tinvest: recording a listing's currency failed",
				"instrument_uid", l.InstrumentUID, "err", err)
			continue
		}
		listings[i].Currency = currency
	}
	return listings
}
