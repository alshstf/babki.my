package tinvest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/riverqueue/river"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/secretbox"
)

// A connected broker is a quote source, and for this portfolio the best one:
// no second key, the owner's own broker's data, and it covers what MOEX's feed
// does not:
//
//   - foreign shares (Apple on 2026-08-08: 313,25 $ against 313,33 $ on
//     Nasdaq);
//   - delisted funds with a dealer's price (FinEx, untraded on any exchange
//     since 2023, still quoted over the counter, the only price they sell at);
//   - papers that stopped trading, priced with the day of their last trade
//     (Tesla's old rouble line: 2022-02-25), so staleness shows in the date.
//
// The MOEX feed is unchanged: both write the same table, and on the same paper
// and day the later run wins. They are two observations of one price, a few
// hundredths of a percent apart, and every row carries its source.

// lastPricesBatch is how many instruments one GetLastPrices asks about. No
// ceiling is documented (and none probed against a live broker); 100 keeps this
// portfolio to one request, as the documented limit is per minute.
const lastPricesBatch = 100

// LastPrice is an instrument's latest price at the broker. The instant is
// the broker's, years ago for a paper that stopped trading, and is never
// replaced by a fresher-looking day (#90). Dealer means the broker quoted it as
// a dealer, not an exchange: for a delisted fund the only price, but a different
// fact.
type LastPrice struct {
	InstrumentUID string
	Price         decimal.Decimal
	At            time.Time
	Dealer        bool
}

// LastPrices returns each instrument's latest price. An entry without a price
// (the broker does send them: a FinEx id answers with no price, figi or ticker)
// is left out rather than returned as zero.
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

// RefreshQuotesArgs is the periodic job pricing every active connection's
// mapped instruments.
type RefreshQuotesArgs struct{}

func (RefreshQuotesArgs) Kind() string { return "tinvest.refresh_quotes" }

// quoteStore is the narrow view of marketdata.Store this worker needs.
type quoteStore interface {
	UpsertQuotes(ctx context.Context, quotes []marketdata.Quote) error
	UpsertBondDays(ctx context.Context, days []marketdata.BondDay) error
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

// NewQuotesWorker builds the worker that stores broker prices. now feeds the
// future-date guard; nil for time.Now.
func NewQuotesWorker(store *Store, quotes quoteStore, box *secretbox.Box, newClient clientFactory, log *slog.Logger, now func() time.Time) river.Worker[RefreshQuotesArgs] {
	if now == nil {
		now = time.Now
	}
	return &quotesWorker{store: store, quotes: quotes, box: box, newClient: newClient, log: log, now: now}
}

func (w *quotesWorker) Timeout(*river.Job[RefreshQuotesArgs]) time.Duration {
	return 5 * time.Minute
}

// Work prices every active connection's mapped listings. One connection's
// failure does not stop the others; the last error is returned at the end so
// River retries. A revoked token marks the connection instead, as the sync worker
// does: retrying cannot un-revoke it.
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

// markRevoked records the broker's refusal; a failed write is logged so it
// cannot stop the other connections.
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

	token, err := w.box.Open(conn.TokenCiphertext)
	if err != nil {
		return 0, fmt.Errorf("tinvest: open token of connection %s: %w", conn.ID, err)
	}
	client, err := w.newClient(string(token))
	if err != nil {
		return 0, err
	}

	// Currencies of listings recorded before migration 0017, learned first,
	// so a listing is priced in its own currency or not at all, never the
	// catalog row's.
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
	// No mapped listing is ordinary and must not skip the hand-entered pass
	// below.
	var prices []LastPrice
	if len(uids) > 0 {
		var err error
		if prices, err = client.LastPrices(ctx, uids); err != nil {
			return 0, err
		}
	}

	today := mskDay(w.now())
	quotes := make([]marketdata.Quote, 0, len(prices))
	var bonds []QuotableInstrument
	for _, p := range prices {
		listing, ok := byUID[p.InstrumentUID]
		if !ok {
			// A price nobody asked for: nothing to store it against.
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
			// The exchange feed's guard: the latest quote is ORDER BY on_date DESC,
			// so a future-dated row would outrank every real one until that date.
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
		if listing.Bond {
			bonds = append(bonds, listing)
		}
	}
	// Holdings nobody imported, priced by search; their failure does not
	// cost the mapped prices already in hand.
	unmapped, unmappedErr := w.priceUnmapped(ctx, conn, client)
	quotes = append(quotes, unmapped...)

	if len(quotes) == 0 {
		return 0, unmappedErr
	}
	if err := w.quotes.UpsertQuotes(ctx, quotes); err != nil {
		return 0, err
	}
	if err := w.quotes.UpsertBondDays(ctx, w.bondDays(ctx, client, bonds, today)); err != nil {
		return 0, err
	}
	return len(quotes), unmappedErr
}

// bondDays asks the broker for each priced bond's current nominal and accrued
// interest, which its percentage price applies to. A bond the broker does not
// answer for keeps whatever the exchange stated; its price stands either way.
// The interest is kept only in the nominal's currency.
func (w *quotesWorker) bondDays(ctx context.Context, client *Client, bonds []QuotableInstrument, today time.Time) []marketdata.BondDay {
	var out []marketdata.BondDay
	for _, b := range bonds {
		nominal, accrued, err := client.BondTermsByUID(ctx, b.InstrumentUID)
		if err != nil {
			w.log.Warn("tinvest: the broker did not state a bond's nominal and accrued interest; its price stands without them",
				"instrument_uid", b.InstrumentUID, "err", err)
			continue
		}
		if !nominal.Decimal().IsPositive() {
			// Redeemed: nothing left to value.
			continue
		}
		day := marketdata.BondDay{
			InstrumentID: b.InstrumentID, On: today, Face: nominal.Decimal(),
			Currency: strings.ToUpper(nominal.Currency), Source: SourceExchange,
		}
		if a := accrued.Decimal(); strings.EqualFold(accrued.Currency, nominal.Currency) && !a.IsNegative() {
			day.Accrued = &a
		}
		out = append(out, day)
	}
	return out
}

// unmappedSearchesPerRun bounds broker searches for hand-entered holdings,
// a small set by nature; the cap guards against a strange catalog, and anything
// left out is logged.
const unmappedSearchesPerRun = 25

// priceUnmapped prices holdings this connection never imported: per catalog
// row it searches the broker by ISIN and decides which answer is the same paper
// (candidateListings, pickListing). Nothing is remembered: writing a guessed
// listing into tinvest_instrument_map would put it on the import's resolution
// path, where a wrong guess files a stranger's trades against the owner's
// paper.
func (w *quotesWorker) priceUnmapped(ctx context.Context, conn Connection, client *Client) ([]marketdata.Quote, error) {
	want, err := w.store.UnmappedHeldInstruments(ctx, conn.SpaceID, conn.ID)
	if err != nil {
		return nil, err
	}
	if len(want) > unmappedSearchesPerRun {
		w.log.Info("tinvest: more hand-entered holdings than one run searches for, the rest wait for the next",
			"connection_id", conn.ID, "held", len(want), "searched", unmappedSearchesPerRun)
		want = want[:unmappedSearchesPerRun]
	}

	today := mskDay(w.now())
	out := []marketdata.Quote{}
	for _, u := range want {
		listing, price, currency, ok, err := resolveListing(ctx, client, w.log, u)
		if err != nil {
			return out, err
		}
		if !ok {
			continue
		}
		on := mskDay(price.At)
		if on.After(today) {
			w.log.Warn("tinvest: refusing a price dated in the future",
				"instrument_uid", listing.UID, "on", on.Format(time.DateOnly))
			continue
		}
		out = append(out, marketdata.Quote{
			InstrumentID: u.InstrumentID,
			On:           on,
			Price:        price.Price,
			Currency:     currency,
			Source:       quoteSource(price.Dealer),
		})
	}
	return out, nil
}

// resolveListing finds the listing of an unmapped holding by ISIN: the
// candidates that are this paper, the one still quoted in the holding's currency
// (pickListing), each candidate's currency asked of its passport freshest first,
// since the search reports none. ok is false, with the reason logged, without an
// ISIN or a choosable listing; err only for a revoked token.
func resolveListing(ctx context.Context, client *Client, log *slog.Logger, u UnmappedHeldInstrument) (Listing, LastPrice, string, bool, error) {
	if u.ISIN == "" {
		// Nothing to search by; a ticker routinely finds another issuer.
		log.Debug("tinvest: a holding with no ISIN cannot be looked up at the broker",
			"instrument_id", u.InstrumentID, "ticker", u.Ticker)
		return Listing{}, LastPrice{}, "", false, nil
	}
	found, err := client.FindInstruments(ctx, u.ISIN)
	if err != nil {
		if errors.Is(err, ErrTokenInvalid) {
			return Listing{}, LastPrice{}, "", false, err
		}
		log.Debug("tinvest: searching the broker for a hand-entered holding failed",
			"instrument_id", u.InstrumentID, "isin", u.ISIN, "err", err)
		return Listing{}, LastPrice{}, "", false, nil
	}
	candidates := candidateListings(u, found)
	if len(candidates) == 0 {
		log.Debug("tinvest: the broker lists nothing that is this paper in this currency",
			"instrument_id", u.InstrumentID, "isin", u.ISIN, "currency", u.Currency, "found", len(found))
		return Listing{}, LastPrice{}, "", false, nil
	}
	uids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		uids = append(uids, c.UID)
	}
	prices, err := client.LastPrices(ctx, uids)
	if err != nil {
		if errors.Is(err, ErrTokenInvalid) {
			return Listing{}, LastPrice{}, "", false, err
		}
		log.Debug("tinvest: asking the broker for a hand-entered holding's price failed",
			"instrument_id", u.InstrumentID, "err", err)
		return Listing{}, LastPrice{}, "", false, nil
	}
	byUID := make(map[string]LastPrice, len(prices))
	for _, p := range prices {
		byUID[p.InstrumentUID] = p
	}
	// Freshest first; a listing in another currency is set aside and the
	// next asked, so a rouble line does not hide a dollar line (#261).
	remaining := candidates
	for {
		listing, price, ok := pickListing(remaining, byUID)
		if !ok {
			log.Debug("tinvest: no listing of this paper can be chosen without guessing, leaving it unpriced",
				"instrument_id", u.InstrumentID, "isin", u.ISIN, "candidates", len(candidates))
			return Listing{}, LastPrice{}, "", false, nil
		}
		brief, err := client.InstrumentByUID(ctx, listing.UID)
		if err != nil {
			if errors.Is(err, ErrTokenInvalid) {
				return Listing{}, LastPrice{}, "", false, err
			}
			log.Debug("tinvest: could not learn what the chosen listing is denominated in",
				"instrument_uid", listing.UID, "err", err)
			return Listing{}, LastPrice{}, "", false, nil
		}
		currency := upperCurrency(brief.Currency)
		if currency != "" && strings.EqualFold(currency, u.Currency) {
			return listing, price, currency, true, nil
		}
		log.Debug("tinvest: a listing is denominated in another currency than the holding, trying the next",
			"instrument_id", u.InstrumentID, "listing", currency, "holding", u.Currency)
		remaining = slices.DeleteFunc(slices.Clone(remaining), func(l Listing) bool { return l.UID == listing.UID })
	}
}

// SourceExchange and SourceDealer say where the broker's price came from:
// an exchange struck it, or the broker stands behind it as a dealer.
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

// fillCurrencies learns each currency-less listing's currency from its
// passport and remembers it. A failure costs that listing its price this run;
// it is never priced under the catalog row's currency.
func (w *quotesWorker) fillCurrencies(ctx context.Context, conn Connection, src passportSource, listings []QuotableInstrument) []QuotableInstrument {
	for i, l := range listings {
		if l.Currency != "" {
			continue
		}
		brief, err := src.InstrumentByUID(ctx, l.InstrumentUID)
		if err != nil {
			if errors.Is(err, ErrTokenInvalid) {
				// Nothing further will work; the caller's own request surfaces it.
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

// brokerInstrumentKinds maps the search's instrument kind to the catalog's
// type: FindInstrument's spelling of brokerInstrumentTypes.
var brokerInstrumentKinds = map[string]instrument.Type{
	"INSTRUMENT_TYPE_SHARE": instrument.TypeShare,
	"INSTRUMENT_TYPE_BOND":  instrument.TypeBond,
	"INSTRUMENT_TYPE_ETF":   instrument.TypeETF,
}

// candidateListings are a security's listings that could stand for a catalog
// row: same ISIN and same kind of asset, never by ticker ("T" is one issuer's
// bond and another's share). Currency is not checked here because the search has
// none (see Listing); priceUnmapped checks the winner's passport.
func candidateListings(want UnmappedHeldInstrument, found []Listing) []Listing {
	out := []Listing{}
	for _, l := range found {
		if !strings.EqualFold(l.ISIN, want.ISIN) {
			continue
		}
		if kind, ok := brokerInstrumentKinds[l.Kind]; !ok || string(kind) != want.Type {
			continue
		}
		out = append(out, l)
	}
	return out
}

// pickListing chooses which listing a price comes from, or refuses. The
// freshest price wins: Apple has four listings, two quoted this week and two
// frozen since 2022, and no list of venue names is needed. A same-day tie is fine
// only at the same price; different prices are a choice without grounds, so the
// holding stays visibly unpriced.
func pickListing(candidates []Listing, prices map[string]LastPrice) (Listing, LastPrice, bool) {
	var best Listing
	var bestPrice LastPrice
	tied := false
	for _, c := range candidates {
		p, ok := prices[c.UID]
		if !ok || !p.Price.IsPositive() {
			continue
		}
		switch {
		case bestPrice.At.IsZero() || p.At.After(bestPrice.At):
			best, bestPrice, tied = c, p, false
		case p.At.Equal(bestPrice.At) && !p.Price.Equal(bestPrice.Price):
			tied = true
		}
	}
	if bestPrice.At.IsZero() || tied {
		return Listing{}, LastPrice{}, false
	}
	return best, bestPrice, true
}
