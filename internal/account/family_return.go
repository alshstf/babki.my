package account

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
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

// familyReturn reckons the period over the journal-valued brokerage accounts;
// other accounts have no record of what crossed their edge. Moves between them
// cancel out: both legs are valued on the same day at the same price.
func (h *Handler) familyReturn(ctx context.Context, spaceID uuid.UUID, base string, from, to time.Time) (ReturnBasis, int, error) {
	b, ids, err := h.familyReturnOver(ctx, spaceID, base, from, to)
	return b, len(ids), err
}

// familyReturnOver is familyReturn with the accounts it counted.
func (h *Handler) familyReturnOver(ctx context.Context, spaceID uuid.UUID, base string, from, to time.Time) (ReturnBasis, []uuid.UUID, error) {
	accounts, err := h.store.ListWithBalance(ctx, spaceID)
	if err != nil {
		return ReturnBasis{}, nil, err
	}
	vals, err := h.valuations(ctx, spaceID, accounts, base, time.Now().UTC(), marketdata.NewRateMemo(h.converter))
	if err != nil {
		return ReturnBasis{}, nil, err
	}
	out := ReturnBasis{Complete: true}
	var counted []uuid.UUID
	for _, a := range accounts {
		// A card's money in and out is spending and earning, not an
		// investment's; it is left out even when kept by its operations.
		v, ok := vals[a.ID]
		if !ok || !v.byJournal || v.everyday {
			continue
		}
		counted = append(counted, a.ID)
		b, err := h.journals.ReturnBasis(ctx, spaceID, a.ID, from, to)
		if err != nil {
			return ReturnBasis{}, nil, fmt.Errorf("the return of account %s: %w", a.ID, err)
		}
		if out.Start.Minor, err = money.Add(out.Start.Minor, b.Start.Minor); err != nil {
			return ReturnBasis{}, nil, err
		}
		if out.End.Minor, err = money.Add(out.End.Minor, b.End.Minor); err != nil {
			return ReturnBasis{}, nil, err
		}
		out.Complete = out.Complete && b.Complete
		out.Flows = append(out.Flows, b.Flows...)
	}
	return out, counted, nil
}

// familyPerformance is the family's time-weighted rate, drawdown and
// volatility over the period (#405): the counted accounts' full worth summed
// on the period's start, every month's last day, each day money crossed the
// family's edge, and its end. Nothing is told when a day could not be valued
// in full.
func (h *Handler) familyPerformance(ctx context.Context, spaceID uuid.UUID, ids []uuid.UUID, flows []ReturnFlow, from, to time.Time) (twr.Performance, error) {
	byDay := map[time.Time]int64{}
	var flowDays []time.Time
	for _, f := range flows {
		if _, seen := byDay[f.Day]; !seen {
			flowDays = append(flowDays, f.Day)
		}
		byDay[f.Day] += f.Minor
	}
	days, monthEnds := twr.Days(from, to, flowDays)
	worth := make([]int64, len(days))
	for _, id := range ids {
		values, err := h.journals.ValuesOn(ctx, spaceID, id, days)
		if err != nil {
			return twr.Performance{}, err
		}
		for i, v := range values {
			if v.FullUnpriced != 0 || len(v.FullMissingRates) != 0 {
				return twr.Performance{}, nil
			}
			if worth[i], err = money.Add(worth[i], v.FullMinor); err != nil {
				return twr.Performance{}, err
			}
		}
	}
	points := make([]twr.Point, len(days))
	for i, day := range days {
		points[i] = twr.Point{Day: day, Worth: worth[i], Flow: byDay[day]}
	}
	points[0].Flow = 0
	return twr.Measure(points, monthEnds), nil
}

// FamilyReturnBasis is the family's worth at both ends of the period and
// what crossed its edge in between, in the base currency — what GET
// /api/v1/return reckons from — for weighing it against a benchmark (#401).
func (h *Handler) FamilyReturnBasis(ctx context.Context, spaceID uuid.UUID, base string, from, to time.Time) (ReturnBasis, error) {
	b, _, err := h.familyReturn(ctx, spaceID, base, from, to)
	return b, err
}

func (h *Handler) handleFamilyReturn(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	from, errFrom := time.Parse(time.DateOnly, r.URL.Query().Get("from"))
	to, errTo := time.Parse(time.DateOnly, r.URL.Query().Get("to"))
	if errFrom != nil || errTo != nil || !from.Before(to) || to.After(dates.LatestRecordable()) {
		httpjson.Error(w, http.StatusBadRequest, "from and to must be YYYY-MM-DD, from before to, to at most today")
		return
	}
	sp, err := h.spaces.SpaceByID(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	b, ids, err := h.familyReturnOver(r.Context(), p.SpaceID, sp.BaseCurrency, from, to)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	counted := len(ids)
	var put int64
	for _, f := range b.Flows {
		if put, err = money.Sub(put, f.Minor); err != nil {
			family.WriteError(w, err)
			return
		}
	}
	profit, err := money.Sub(b.End.Minor, b.Start.Minor)
	if err == nil {
		profit, err = money.Sub(profit, put)
	}
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := apitypes.FamilyReturn{
		Currency: sp.BaseCurrency, From: from.Format(time.DateOnly), To: to.Format(time.DateOnly),
		StartMinor: b.Start.Minor, EndMinor: b.End.Minor, ContributionsMinor: put, ProfitMinor: profit,
		AnnualRate: nullable.NewNullNullable[string](), Complete: b.Complete, Accounts: counted,
		TimeWeightedRate: nullable.NewNullNullable[string](), TimeWeightedPeriod: nullable.NewNullNullable[string](),
		MaxDrawdown: nullable.NewNullNullable[string](), DrawdownFrom: nullable.NewNullNullable[string](),
		DrawdownTo: nullable.NewNullNullable[string](), Volatility: nullable.NewNullNullable[string](),
	}
	if rate, ok := annualRate(b, from, to); ok {
		out.AnnualRate = nullable.NewNullableWithValue(decimal.NewFromFloat(rate).Round(4).String())
	}
	if b.Complete {
		perf, err := h.familyPerformance(r.Context(), p.SpaceID, ids, b.Flows, from, to)
		if err != nil {
			family.WriteError(w, err)
			return
		}
		round := func(f float64) string { return decimal.NewFromFloat(f).Round(4).String() }
		if perf.HasRate {
			out.TimeWeightedPeriod = nullable.NewNullableWithValue(round(perf.Period))
			out.TimeWeightedRate = nullable.NewNullableWithValue(round(twr.Annual(perf.Period, from, to)))
		}
		if perf.HasDrawdown {
			out.MaxDrawdown = nullable.NewNullableWithValue(round(perf.Drawdown))
			out.DrawdownFrom = nullable.NewNullableWithValue(perf.Peak.Format(time.DateOnly))
			out.DrawdownTo = nullable.NewNullableWithValue(perf.Bottom.Format(time.DateOnly))
		}
		if perf.HasVolatility {
			out.Volatility = nullable.NewNullableWithValue(round(perf.Volatility))
		}
	}
	httpjson.Write(w, http.StatusOK, out)
}

// annualRate is the money-weighted annual rate of a basis (see xirr.Rate).
func annualRate(b ReturnBasis, from, to time.Time) (float64, bool) {
	flows := make([]xirr.Flow, 0, len(b.Flows)+2)
	if b.Start.Minor != 0 {
		flows = append(flows, xirr.Flow{Day: from, Amount: -float(b.Start.Minor)})
	}
	for _, f := range b.Flows {
		flows = append(flows, xirr.Flow{Day: f.Day, Amount: float(f.Minor)})
	}
	flows = append(flows, xirr.Flow{Day: to, Amount: float(b.End.Minor)})
	return xirr.Rate(flows)
}

func float(minor int64) float64 {
	f, _ := decimal.NewFromInt(minor).Float64()
	return f
}
