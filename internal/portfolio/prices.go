package portfolio

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
)

// The two valuations (decision Р-11). A holding's liquid price is what the
// market pays for it: the latest market price — a price stated by hand is not
// one — no older than a window. Its full price is, as the space chooses, a
// fund's net asset value or a foreign share's home-exchange close, and past
// those its latest price of any other source.

// priceWindows is how old a market price may be for each valuation; zero is
// any age.
type priceWindows struct{ liquid, full int }

var (
	// today: past a month a paper has stopped trading (a fund frozen since 2022);
	// within it is a long holiday or a thinly traded bond. The full valuation
	// keeps the last price of a paper nothing else prices, dated on screen.
	todayWindows = priceWindows{liquid: 31, full: 0}
	// a past day: a long weekend or the New Year holidays, not a paper whose
	// prices stopped — a return is not reckoned from a price months old.
	pastWindows = priceWindows{liquid: 10, full: 10}
)

// pricedQuote is the price a full valuation uses and where it came from.
type pricedQuote struct {
	marketdata.Quote
	source apitypes.PriceSource
}

// priceBook is every price one valuation day needs, per paper.
type priceBook struct {
	liquid map[uuid.UUID]marketdata.Quote
	full   map[uuid.UUID]pricedQuote
	// lastMarket is the latest market price of any age, to say since when a
	// paper has not traded.
	lastMarket map[uuid.UUID]marketdata.Quote
}

// bondDayStore is where a bond's face and accrued interest by day are read.
type bondDayStore interface {
	BondDaysOn(ctx context.Context, ids []uuid.UUID, day time.Time) (map[uuid.UUID]marketdata.BondDay, error)
}

// accruedWindow is how old a bond's stated interest may be: it grows every
// day and drops at each coupon, so an old figure is no figure.
const accruedWindow = 10

// priceStore is the price store as both valuations read it.
type priceStore interface {
	QuotesOn(ctx context.Context, instrumentIDs []uuid.UUID, day time.Time) (map[uuid.UUID]marketdata.Quote, error)
	MarketQuotesOn(ctx context.Context, instrumentIDs []uuid.UUID, day time.Time) (map[uuid.UUID]marketdata.Quote, error)
	ReferencePricesOn(ctx context.Context, ids []uuid.UUID, day time.Time) (map[uuid.UUID]map[marketdata.ReferenceKind]marketdata.ReferencePrice, error)
}

// pricesOn is the prices of ids on day under the space's setting. A store
// keeping only the latest prices (a test's) is read as if every price were a
// market one on its own day.
func (s *Service) pricesOn(ctx context.Context, ids []uuid.UUID, day time.Time, setting family.FullValuation, windows priceWindows) (priceBook, error) {
	book := priceBook{
		liquid:     make(map[uuid.UUID]marketdata.Quote, len(ids)),
		full:       make(map[uuid.UUID]pricedQuote, len(ids)),
		lastMarket: make(map[uuid.UUID]marketdata.Quote, len(ids)),
	}
	if len(ids) == 0 {
		return book, nil
	}
	var (
		latest, market map[uuid.UUID]marketdata.Quote
		refs           map[uuid.UUID]map[marketdata.ReferenceKind]marketdata.ReferencePrice
		err            error
	)
	if ps, ok := s.quotes.(priceStore); ok {
		if latest, err = ps.QuotesOn(ctx, ids, day); err != nil {
			return priceBook{}, err
		}
		if market, err = ps.MarketQuotesOn(ctx, ids, day); err != nil {
			return priceBook{}, err
		}
		if setting != family.FullValuationLiquid {
			if refs, err = ps.ReferencePricesOn(ctx, ids, day); err != nil {
				return priceBook{}, err
			}
		}
	} else {
		if latest, err = s.quotes.LatestQuotes(ctx, ids); err != nil {
			return priceBook{}, err
		}
		market = make(map[uuid.UUID]marketdata.Quote, len(latest))
		for id, q := range latest {
			if q.Source != ManualPriceSource {
				market[id] = q
			}
		}
	}

	within := func(q marketdata.Quote, days int) bool {
		return days == 0 || !q.On.Before(day.AddDate(0, 0, -days))
	}
	for _, id := range ids {
		if q, ok := market[id]; ok {
			book.lastMarket[id] = q
			if within(q, windows.liquid) {
				book.liquid[id] = q
			}
		}
		if setting == family.FullValuationLiquid {
			if q, ok := book.liquid[id]; ok {
				book.full[id] = pricedQuote{Quote: q, source: apitypes.PriceSourceMarket}
			}
			continue
		}
		if r, ok := refs[id][marketdata.ReferenceNAV]; ok {
			book.full[id] = pricedQuote{Quote: referenceQuote(r), source: apitypes.PriceSourceNav}
			continue
		}
		if r, ok := refs[id][marketdata.ReferenceForeign]; ok && setting == family.FullValuationNAVAndForeign {
			book.full[id] = pricedQuote{Quote: referenceQuote(r), source: apitypes.PriceSourceForeign}
			continue
		}
		if q, ok := latest[id]; ok {
			source := apitypes.PriceSourceMarket
			if q.Source == ManualPriceSource {
				source = apitypes.PriceSourceManual
			}
			// A price stated by hand is the person's word, at any age.
			if source == apitypes.PriceSourceManual || within(q, windows.full) {
				book.full[id] = pricedQuote{Quote: q, source: source}
			}
		}
	}
	return book, s.attachBondDays(ctx, ids, day, book)
}

// attachBondDays gives each bond's quotes its face and accrued interest on
// day (Quote.Bond): the face of any age, the interest only within
// accruedWindow.
func (s *Service) attachBondDays(ctx context.Context, ids []uuid.UUID, day time.Time, book priceBook) error {
	bs, ok := s.quotes.(bondDayStore)
	if !ok {
		return nil
	}
	days, err := bs.BondDaysOn(ctx, ids, day)
	if err != nil {
		return err
	}
	for id, d := range days {
		if d.On.Before(day.AddDate(0, 0, -accruedWindow)) {
			d.Accrued = nil
		}
		if q, ok := book.liquid[id]; ok {
			q.Bond = &d
			book.liquid[id] = q
		}
		if q, ok := book.full[id]; ok {
			q.Bond = &d
			book.full[id] = q
		}
	}
	return nil
}

// fullQuotes is the full prices as plain quotes, for code valuing one way.
func (b priceBook) fullQuotes() map[uuid.UUID]marketdata.Quote {
	out := make(map[uuid.UUID]marketdata.Quote, len(b.full))
	for id, q := range b.full {
		out[id] = q.Quote
	}
	return out
}

func referenceQuote(r marketdata.ReferencePrice) marketdata.Quote {
	return marketdata.Quote{InstrumentID: r.InstrumentID, On: r.On, Price: r.Price, Currency: r.Currency, Source: r.Source}
}

// addLiquid publishes, beside a position's full valuation, where its price
// came from and what it can be sold for now (decision Р-11).
func (s *Service) addLiquid(ctx context.Context, out *apitypes.Position, p *Position, inst instrument.Instrument,
	book priceBook, base string, now time.Time, rates *marketdata.RateMemo,
) error {
	out.PriceSource = nullable.NewNullNullable[apitypes.PriceSource]()
	if !out.Price.IsNull() && out.Price.IsSpecified() {
		out.PriceSource = nullable.NewNullableWithValue(book.full[p.InstrumentID].source)
	}
	out.LiquidValueMinor = nullable.NewNullNullable[int64]()
	out.LiquidValueInBaseMinor = nullable.NewNullNullable[int64]()
	out.LastTradedOn = nullable.NewNullNullable[string]()
	if !p.Quantity.IsPositive() {
		return nil
	}
	q, ok := book.liquid[p.InstrumentID]
	if !ok {
		if last, traded := book.lastMarket[p.InstrumentID]; traded {
			out.LastTradedOn = nullable.NewNullableWithValue(last.On.Format(time.DateOnly))
		}
		return nil
	}
	minor, currency, gap, err := marketValue(inst.Type, inst.FaceValueMinor, inst.FaceCurrency, p.Quantity, q, true)
	if err != nil || gap != valuationStruck {
		return err
	}
	inPosition, ok, err := convertAt(ctx, rates, minor, currency, p.Currency, now)
	if err != nil || !ok {
		return err
	}
	out.LiquidValueMinor = nullable.NewNullableWithValue(inPosition)
	inBase, ok, err := convertAt(ctx, rates, minor, currency, base, now)
	if err != nil || !ok {
		return err
	}
	out.LiquidValueInBaseMinor = nullable.NewNullableWithValue(inBase)
	return nil
}

// convertAt converts minor from one currency to another at day's rate; ok is
// false when there is no rate. An overflow is an error.
func convertAt(ctx context.Context, rates *marketdata.RateMemo, minor int64, from, to string, day time.Time) (int64, bool, error) {
	if from == to {
		return minor, true, nil
	}
	rl := rates.Rate(ctx, from, to, day)
	if rl.Err != nil {
		if errors.Is(rl.Err, marketdata.ErrNoRate) {
			return 0, false, nil
		}
		return 0, false, rl.Err
	}
	converted, err := applyRate(rl, minor)
	if err != nil {
		return 0, false, err
	}
	return converted, true, nil
}
