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
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/dates"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/money"
	"babki.my/babki/internal/platform/xirr"
)

// familyReturn reckons the period over the accounts the total counts by their
// journal — the brokerage accounts; a deposit or a card has a balance and no
// record of what crossed its edge. A move of shares between two of these
// accounts needs no special case: its legs are valued on the same day at the
// same price, and what one puts in the other takes out.
func (h *Handler) familyReturn(ctx context.Context, spaceID uuid.UUID, base string, from, to time.Time) (ReturnBasis, int, error) {
	accounts, err := h.store.ListWithBalance(ctx, spaceID)
	if err != nil {
		return ReturnBasis{}, 0, err
	}
	vals, err := h.valuations(ctx, spaceID, accounts, base, time.Now().UTC(), make(map[rateKey]*rateLookup))
	if err != nil {
		return ReturnBasis{}, 0, err
	}
	out := ReturnBasis{Complete: true}
	counted := 0
	for _, a := range accounts {
		v, ok := vals[a.ID]
		if !ok || !v.byJournal {
			continue
		}
		counted++
		b, err := h.journals.ReturnBasis(ctx, spaceID, a.ID, from, to)
		if err != nil {
			return ReturnBasis{}, 0, fmt.Errorf("the return of account %s: %w", a.ID, err)
		}
		if out.Start.Minor, err = money.Add(out.Start.Minor, b.Start.Minor); err != nil {
			return ReturnBasis{}, 0, err
		}
		if out.End.Minor, err = money.Add(out.End.Minor, b.End.Minor); err != nil {
			return ReturnBasis{}, 0, err
		}
		out.Complete = out.Complete && b.Complete
		out.Flows = append(out.Flows, b.Flows...)
	}
	return out, counted, nil
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
	b, counted, err := h.familyReturn(r.Context(), p.SpaceID, sp.BaseCurrency, from, to)
	if err != nil {
		family.WriteError(w, err)
		return
	}
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
	}
	if rate, ok := annualRate(b, from, to); ok {
		out.AnnualRate = nullable.NewNullableWithValue(decimal.NewFromFloat(rate).Round(4).String())
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
