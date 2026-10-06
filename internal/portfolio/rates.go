package portfolio

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/money"
)

// applyRate converts amountMinor at rl's rate, rounding once half away from
// zero, as marketdata.Converter.Convert does (only tests hold the two
// together). An overflow is refused (#27), as a request error rather than a
// null. Sums of many terms round once for the whole sum instead (sumInBase).
func applyRate(rl marketdata.RateResult, amountMinor int64) (int64, error) {
	minor, err := money.Minor(decimal.NewFromInt(amountMinor).Mul(rl.Rate))
	if err != nil {
		return 0, fmt.Errorf("%w: %d at a rate of %s", err, amountMinor, rl.Rate)
	}
	return minor, nil
}

// datedMinor is one amount, its currency and the date whose rate values it.
// The currency is per term because one sum's terms may differ (income in
// several currencies).
type datedMinor struct {
	minor int64
	from  string
	on    time.Time
}

// sumInBase converts every amount at its own date's rate out of its own
// currency and rounds the total once. ok is false when some rate is missing,
// and the caller publishes nothing. err is a real failure, including a total
// too large for int64.
func (s *Service) sumInBase(ctx context.Context, amounts []datedMinor, to string, rates *marketdata.RateMemo) (minor int64, ok bool, err error) {
	total := decimal.Zero
	for _, a := range amounts {
		rl := rates.Rate(ctx, a.from, to, a.on)
		if rl.Err != nil {
			if errors.Is(rl.Err, marketdata.ErrNoRate) {
				return 0, false, nil
			}
			return 0, false, rl.Err
		}
		total = total.Add(decimal.NewFromInt(a.minor).Mul(rl.Rate))
	}
	minor, err = money.Minor(total)
	if err != nil {
		return 0, false, fmt.Errorf("%w: %d terms totalling %s %s", err, len(amounts), total, to)
	}
	return minor, true, nil
}

// lotTerms turns the held lots into one sum's terms, each dated by the lot's
// purchase (or settlement) day. dated is false when a lot's purchase date is
// unknown; no date is invented. Both the sum and the prefetch use it. It asks
// anyUndatedLot first, so the published flags and this answer are one
// predicate, and the dereference below is safe.
func lotTerms(lots []Lot, currency string) (terms []datedMinor, dated bool) {
	if anyUndatedLot(lots) {
		return nil, false
	}
	terms = make([]datedMinor, 0, len(lots))
	for _, l := range lots {
		if l.AcquiredOn == nil {
			// Bought for nothing as far as anyone knows: no term, no rate.
			continue
		}
		// A lot's cost is in the position's currency (Type.mustMatchPositionCurrency).
		on := *l.AcquiredOn
		if l.RateOn != nil {
			on = *l.RateOn
		}
		terms = append(terms, datedMinor{minor: l.CostMinor, from: currency, on: on})
	}
	return terms, true
}

// incomeTerms turns income operations into one sum's terms, each at its own
// day's rate and out of the currency it arrived in. No undated case.
func incomeTerms(income []Operation) []datedMinor {
	terms := make([]datedMinor, 0, len(income))
	for _, o := range income {
		terms = append(terms, datedMinor{minor: o.AmountMinor, from: o.Currency, on: o.OccurredOn})
	}
	return terms
}

// realizedTerms turns the disposals into one sum's terms: proceeds and fee at
// the disposal's (settlement) day, each retired parcel at its purchase day (НК
// РФ ст. 210 п. 5), so the currency's move between purchase and sale stays in
// the result. dated is false when a retired parcel's purchase date is unknown;
// the caller then publishes nothing rather than an inflated profit. Like
// lotTerms it asks anyUndatedRealization first.
func realizedTerms(events []Realization, currency string) (terms []datedMinor, dated bool) {
	if anyUndatedRealization(events) {
		return nil, false
	}
	terms = make([]datedMinor, 0, len(events)*2)
	for _, e := range events {
		// Proceeds and fee are in the disposal's settlement currency, the basis in the
		// position's; every term is converted before summing, so the base figure
		// exists even when the native one does not.
		on := e.OccurredOn
		if e.RateOn != nil {
			on = *e.RateOn
		}
		terms = append(terms,
			datedMinor{minor: e.ProceedsMinor, from: e.Currency, on: on},
			datedMinor{minor: -e.FeeMinor, from: e.Currency, on: on},
		)
		for _, r := range e.Released {
			if r.AcquiredOn == nil {
				// Sold as bought for nothing: the whole proceeds are the result.
				continue
			}
			paid := *r.AcquiredOn
			if r.RateOn != nil {
				paid = *r.RateOn
			}
			terms = append(terms, datedMinor{minor: -r.CostMinor, from: currency, on: paid})
		}
	}
	return terms, true
}

// rateQueries lists every rate the loop will ask for, so one RatesOn call
// resolves them (#40, #53). The dates come from lotTerms, incomeTerms and
// realizedTerms and the valuation pair from marketValue — the same functions
// the figures use — except the valuation's date, which is `now` by hand.
// Completeness is only an optimization: a miss costs a round trip, and a wrong
// query is filed where nothing looks. The list is deduplicated before
// returning, since terms repeat days.
func rateQueries(
	positions map[uuid.UUID]*Position,
	instruments map[uuid.UUID]instrument.Instrument,
	quotes map[uuid.UUID]marketdata.Quote,
	income map[uuid.UUID][]Operation,
	cash map[string]*CashPosition,
	baseCurrency string,
	now time.Time,
) []marketdata.RateQuery {
	var out []marketdata.RateQuery
	// Cash in each currency: today's value and each held parcel's arrival day
	// (see cashToAPI).
	for currency, p := range cash {
		if currency == baseCurrency {
			continue
		}
		out = append(out, marketdata.RateQuery{From: currency, To: baseCurrency, On: now})
		for _, l := range p.Lots {
			out = append(out, marketdata.RateQuery{From: currency, To: baseCurrency, On: l.On})
		}
		// And the departures' days and the arrival days of what they took.
		for _, r := range p.Realizations {
			out = append(out, marketdata.RateQuery{From: currency, To: baseCurrency, On: r.OccurredOn})
			for _, l := range r.Released {
				out = append(out, marketdata.RateQuery{From: currency, To: baseCurrency, On: l.On})
			}
		}
	}
	for _, p := range positions {
		inst, known := instruments[p.InstrumentID]
		q, quoted := quotes[p.InstrumentID]
		if known {
			// marketValue's error is ignored: this only predicts; the loop calls it again
			// and fails where it knows which figure it was building.
			if _, currency, gap, _ := marketValue(inst.Type, inst.FaceValueMinor, inst.FaceCurrency, p.Quantity, q, quoted); gap == valuationStruck {
				if currency != p.Currency {
					// toAPI converts a face-currency valuation into the position's currency: the
					// one lookup whose target is not the base currency.
					out = append(out, marketdata.RateQuery{From: currency, To: p.Currency, On: now})
				}
				if p.Currency != baseCurrency {
					// positionInBase converts the valuation from the same currency (#39). It is
					// asked whenever a valuation exists, including identities, rather than keep a
					// second notion of when it is needed.
					out = append(out, marketdata.RateQuery{From: currency, To: baseCurrency, On: now})
				}
			}
		}
		if p.Currency == baseCurrency {
			// Nothing else converts: both base-currency functions short-circuit.
			continue
		}
		if lots, dated := lotTerms(p.Lots, p.Currency); dated {
			out = appendTermQueries(out, lots, baseCurrency)
		}
		out = appendTermQueries(out, incomeTerms(income[p.InstrumentID]), baseCurrency)
		if realized, dated := realizedTerms(p.Realizations, p.Currency); dated {
			out = appendTermQueries(out, realized, baseCurrency)
		}
	}
	return out
}

// appendTermQueries asks for each term's own date and currency, as sumInBase
// will.
func appendTermQueries(dst []marketdata.RateQuery, terms []datedMinor, to string) []marketdata.RateQuery {
	for _, t := range terms {
		dst = append(dst, marketdata.RateQuery{From: t.from, To: to, On: t.on})
	}
	return dst
}
