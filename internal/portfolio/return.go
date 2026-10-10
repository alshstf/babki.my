package portfolio

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/oapi-codegen/nullable"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/dates"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/money"
	"babki.my/babki/internal/platform/twr"
	"babki.my/babki/internal/platform/xirr"
)

// ReturnFlow is money that crossed an account's edge in a period, signed
// from the investor's side and put into the base currency on its day: put in
// is negative, taken out positive.
type ReturnFlow struct {
	Day   time.Time
	Minor int64
}

// ReturnBasis is what a period's return is reckoned from: the account's worth
// at its start and end, and the money that crossed its edge in between —
// deposits and withdrawals at their day's rate, shares that arrived or left at
// their closing price that day. Income, fees and taxes are not flows: they are
// what the return is made of.
type ReturnBasis struct {
	Currency string
	Start    JournalValue
	End      JournalValue
	Flows    []ReturnFlow
	// Complete is false when some figure could not be had: a holding with no
	// price at either end, a currency with no rate, a moved parcel with no
	// price on its day.
	Complete bool
}

// ReturnBasis reckons the account's period from (exclusive) to to (inclusive).
func (s *Service) ReturnBasis(ctx context.Context, spaceID, accountID uuid.UUID, from, to time.Time) (ReturnBasis, error) {
	values, err := s.ValuesOn(ctx, spaceID, accountID, []time.Time{from, to})
	if err != nil {
		return ReturnBasis{}, err
	}
	sp, err := s.spaces.SpaceByID(ctx, spaceID)
	if err != nil {
		return ReturnBasis{}, err
	}
	// A return is reckoned on the full worth (decision Р-11): on the liquid one
	// a freeze reads as a loss of everything and a thaw as profit from nowhere.
	out := ReturnBasis{Currency: values[0].Currency, Start: fullView(values[0]), End: fullView(values[1])}
	out.Complete = whole(out.Start) && whole(out.End)

	ops, err := s.ops.ListForEngine(ctx, spaceID, accountID)
	if err != nil {
		return ReturnBasis{}, err
	}
	rates := marketdata.NewRateMemo(s.conv)
	for _, o := range ops {
		if !o.OccurredOn.After(from) || o.OccurredOn.After(to) {
			continue
		}
		var (
			minor    int64
			currency string
			known    = true
		)
		switch o.Type {
		case TypeDeposit, TypeWithdrawal:
			minor, currency = -o.AmountMinor, o.Currency
		case TypeTransferIn, TypeTransferOut:
			v, c, ok, err := s.parcelWorth(ctx, sp.FullValuation, o)
			if err != nil {
				return ReturnBasis{}, err
			}
			minor, currency, known = v, c, ok
			if o.Type == TypeTransferIn {
				minor = -minor
			}
		default:
			continue
		}
		flow := ReturnFlow{Day: o.OccurredOn}
		if known && minor != 0 {
			converted, ok, err := s.sumInBase(ctx, []datedMinor{{minor: minor, from: currency, on: o.OccurredOn}}, out.Currency, rates)
			if err != nil {
				return ReturnBasis{}, err
			}
			flow.Minor, known = converted, ok
		}
		if !known {
			out.Complete = false
			continue
		}
		out.Flows = append(out.Flows, flow)
	}
	return out, nil
}

// fullView is v's full worth in the fields a return reads.
func fullView(v JournalValue) JournalValue {
	v.Minor, v.ByCurrency, v.Unpriced, v.MissingRates = v.FullMinor, v.FullByCurrency, v.FullUnpriced, v.FullMissingRates
	return v
}

// whole reports whether a valuation counted everything it holds.
func whole(v JournalValue) bool {
	return v.Unpriced == 0 && len(v.MissingRates) == 0
}

// parcelWorth is what the shares a transfer moved were worth at that day's
// full price, in the currency the price is struck in; ok is false when there
// is none.
func (s *Service) parcelWorth(ctx context.Context, setting family.FullValuation, o Operation) (int64, string, bool, error) {
	if o.InstrumentID == nil || o.Quantity == nil {
		return 0, "", false, nil
	}
	papers, err := s.instruments.ByIDs(ctx, []uuid.UUID{*o.InstrumentID})
	if err != nil {
		return 0, "", false, err
	}
	paper, ok := papers[*o.InstrumentID]
	if !ok {
		return 0, "", false, errInstrumentNotInCatalog
	}
	// The full price, which an account's foreign exchanges do not change.
	book, err := s.pricesOn(ctx, []uuid.UUID{*o.InstrumentID}, o.OccurredOn, setting, pastWindows, false)
	if err != nil {
		return 0, "", false, err
	}
	q, quoted := book.full[*o.InstrumentID]
	minor, currency, gap, err := marketValue(paper.Type, paper.FaceValueMinor, paper.FaceCurrency, *o.Quantity, q.Quote, quoted)
	if err != nil {
		return 0, "", false, err
	}
	return minor, currency, gap == valuationStruck, nil
}

// Profit is what the period earned: the worth at its end, less the worth at
// its start, less the money put in net of what was taken out.
func (b ReturnBasis) Profit() (int64, error) {
	profit, err := money.Sub(b.End.Minor, b.Start.Minor)
	if err != nil {
		return 0, err
	}
	for _, f := range b.Flows {
		// A flow put in is negative from the investor's side, so it is added
		// back here to take it out of the profit.
		if profit, err = money.Add(profit, f.Minor); err != nil {
			return 0, fmt.Errorf("%w: the profit of a period", err)
		}
	}
	return profit, nil
}

// AnnualRate is the money-weighted annual return of the period from to to
// (see xirr.Rate): the worth at the start put in on its first day, the flows on
// theirs, the worth at the end taken out on its last. ok is false when there
// is no rate — nothing put in, nothing at the end.
func (b ReturnBasis) AnnualRate(from, to time.Time) (float64, bool) {
	flows := make([]xirr.Flow, 0, len(b.Flows)+2)
	if b.Start.Minor != 0 {
		flows = append(flows, xirr.Flow{Day: from, Amount: -minorFloat(b.Start.Minor)})
	}
	for _, f := range b.Flows {
		flows = append(flows, xirr.Flow{Day: f.Day, Amount: minorFloat(f.Minor)})
	}
	flows = append(flows, xirr.Flow{Day: to, Amount: minorFloat(b.End.Minor)})
	return xirr.Rate(flows)
}

// minorFloat is an amount of minor units as a float, for the rate search
// only: the rate is a ratio, and no published amount is computed from it.
func minorFloat(minor int64) float64 {
	f, _ := decimal.NewFromInt(minor).Float64()
	return f
}

// Performance is the account's time-weighted rate, drawdown and volatility
// over the period (#405): its full worth at the end of the period's start,
// every month's last day, each day money crossed its edge, and its end
// (twr.Days, twr.Measure). Nothing is told when a day could not be valued in
// full.
func (s *Service) Performance(ctx context.Context, spaceID, accountID uuid.UUID, flows []ReturnFlow, from, to time.Time) (twr.Performance, error) {
	byDay := map[time.Time]int64{}
	var flowDays []time.Time
	for _, f := range flows {
		if _, seen := byDay[f.Day]; !seen {
			flowDays = append(flowDays, f.Day)
		}
		byDay[f.Day] += f.Minor
	}
	days, monthEnds := twr.Days(from, to, flowDays)
	values, err := s.ValuesOn(ctx, spaceID, accountID, days)
	if err != nil {
		return twr.Performance{}, err
	}
	points := make([]twr.Point, len(days))
	for i, v := range values {
		full := fullView(v)
		if !whole(full) {
			return twr.Performance{}, nil
		}
		points[i] = twr.Point{Day: days[i], Worth: full.Minor, Flow: byDay[days[i]]}
	}
	points[0].Flow = 0
	return twr.Measure(points, monthEnds), nil
}

func (h *Handler) handleReturn(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	accountID, ok := pathAccountID(w, r)
	if !ok {
		return
	}
	from, errFrom := time.Parse(time.DateOnly, r.URL.Query().Get("from"))
	to, errTo := time.Parse(time.DateOnly, r.URL.Query().Get("to"))
	if errFrom != nil || errTo != nil || !from.Before(to) || to.After(dates.LatestRecordable()) {
		httpjson.Error(w, http.StatusBadRequest, "from and to must be YYYY-MM-DD, from before to, to at most today")
		return
	}
	basis, err := h.svc.ReturnBasis(r.Context(), p.SpaceID, accountID, from, to)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out, err := periodReturnToAPI(basis, from, to)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	if basis.Complete {
		perf, err := h.svc.Performance(r.Context(), p.SpaceID, accountID, basis.Flows, from, to)
		if err != nil {
			family.WriteError(w, err)
			return
		}
		PerformanceToAPI(&out, perf, from, to)
	}
	httpjson.Write(w, http.StatusOK, out)
}

// periodReturnToAPI publishes a period's basis with its profit and rate.
func periodReturnToAPI(b ReturnBasis, from, to time.Time) (apitypes.PeriodReturn, error) {
	profit, err := b.Profit()
	if err != nil {
		return apitypes.PeriodReturn{}, err
	}
	var put int64
	for _, f := range b.Flows {
		if put, err = money.Sub(put, f.Minor); err != nil {
			return apitypes.PeriodReturn{}, err
		}
	}
	out := apitypes.PeriodReturn{
		Currency: b.Currency, From: from.Format(time.DateOnly), To: to.Format(time.DateOnly),
		StartMinor: b.Start.Minor, EndMinor: b.End.Minor, ContributionsMinor: put, ProfitMinor: profit,
		AnnualRate: nullable.NewNullNullable[string](), Complete: b.Complete,
		TimeWeightedRate: nullable.NewNullNullable[string](), TimeWeightedPeriod: nullable.NewNullNullable[string](),
		MaxDrawdown: nullable.NewNullNullable[string](), DrawdownFrom: nullable.NewNullNullable[string](),
		DrawdownTo: nullable.NewNullNullable[string](), Volatility: nullable.NewNullNullable[string](),
	}
	if rate, ok := b.AnnualRate(from, to); ok {
		out.AnnualRate = nullable.NewNullableWithValue(decimal.NewFromFloat(rate).Round(4).String())
	}
	return out, nil
}

// PerformanceToAPI writes the time-weighted rate, the drawdown and the
// volatility into a period's return.
func PerformanceToAPI(out *apitypes.PeriodReturn, p twr.Performance, from, to time.Time) {
	round := func(f float64) string { return decimal.NewFromFloat(f).Round(4).String() }
	if p.HasRate {
		out.TimeWeightedPeriod = nullable.NewNullableWithValue(round(p.Period))
		out.TimeWeightedRate = nullable.NewNullableWithValue(round(twr.Annual(p.Period, from, to)))
	}
	if p.HasDrawdown {
		out.MaxDrawdown = nullable.NewNullableWithValue(round(p.Drawdown))
		out.DrawdownFrom = nullable.NewNullableWithValue(p.Peak.Format(time.DateOnly))
		out.DrawdownTo = nullable.NewNullableWithValue(p.Bottom.Format(time.DateOnly))
	}
	if p.HasVolatility {
		out.Volatility = nullable.NewNullableWithValue(round(p.Volatility))
	}
}
