package portfolio

import (
	"context"
	"errors"
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
)

// holdingAccounts is the part of operation.Store that says which accounts a
// paper has ever been on.
type holdingAccounts interface {
	AccountsWithInstrument(ctx context.Context, spaceID, instrumentID uuid.UUID) ([]uuid.UUID, error)
}

// priceSeries is the part of marketdata.Store that reads a paper's daily
// prices over a stretch of days.
type priceSeries interface {
	PriceSeries(ctx context.Context, instrumentID uuid.UUID, from, to time.Time) ([]marketdata.Quote, error)
}

// maxPriceYears is how far back a price series may be asked for.
const maxPriceYears = 10

func pathInstrumentID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("instrumentId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid instrumentId")
		return uuid.Nil, false
	}
	return id, true
}

// handleHoldings answers one paper across the family: its position on every
// account whose journal names it, each exactly the row that account's
// positions screen shows for it.
func (h *Handler) handleHoldings(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathInstrumentID(w, r)
	if !ok {
		return
	}
	out, err := h.Holdings(r.Context(), p.SpaceID, id)
	var notComputed journalDoesNotCompute
	switch {
	case errors.As(err, &notComputed):
		httpjson.Error(w, http.StatusUnprocessableEntity, notComputed.Error())
	case errors.Is(err, errInstrumentNotInCatalog):
		httpjson.Error(w, http.StatusNotFound, "not found")
	case err != nil:
		family.WriteError(w, err)
	default:
		httpjson.Write(w, http.StatusOK, out)
	}
}

// Holdings is the paper's catalog row and its positions on the space's
// accounts, ordered as the accounts were found.
func (h *Handler) Holdings(ctx context.Context, spaceID, instrumentID uuid.UUID) (apitypes.InstrumentHoldings, error) {
	papers, err := h.instruments.ByIDs(ctx, []uuid.UUID{instrumentID})
	if err != nil {
		return apitypes.InstrumentHoldings{}, err
	}
	paper, ok := papers[instrumentID]
	if !ok {
		return apitypes.InstrumentHoldings{}, errInstrumentNotInCatalog
	}
	finder, ok := h.ops.(holdingAccounts)
	if !ok {
		return apitypes.InstrumentHoldings{}, errors.New("portfolio: the journal store cannot say which accounts hold a paper")
	}
	accounts, err := finder.AccountsWithInstrument(ctx, spaceID, instrumentID)
	if err != nil {
		return apitypes.InstrumentHoldings{}, err
	}
	out := apitypes.InstrumentHoldings{
		Instrument: instrumentToAPI(paper),
		Holdings:   []apitypes.InstrumentHolding{},
		Total:      nullable.NewNullNullable[apitypes.InstrumentHoldingsTotal](),
	}
	for _, accountID := range accounts {
		resp, _, err := h.positionsResponse(ctx, spaceID, accountID)
		if err != nil {
			return apitypes.InstrumentHoldings{}, err
		}
		for _, pos := range resp.Positions {
			if pos.Instrument.Id == instrumentID {
				out.Holdings = append(out.Holdings, apitypes.InstrumentHolding{AccountId: accountID, Position: pos})
			}
		}
	}
	total, ok, err := holdingsTotal(out.Holdings)
	if err != nil {
		return apitypes.InstrumentHoldings{}, err
	}
	if ok {
		out.Total = nullable.NewNullableWithValue(total)
	}
	return out, nil
}

// holdingsTotal adds the positions up when they are all kept in one currency;
// ok is false otherwise, or when there is nothing to add. A figure that is
// null on any position, or valued in another currency, is null in the total.
func holdingsTotal(holdings []apitypes.InstrumentHolding) (apitypes.InstrumentHoldingsTotal, bool, error) {
	if len(holdings) == 0 {
		return apitypes.InstrumentHoldingsTotal{}, false, nil
	}
	currency := holdings[0].Position.Currency
	quantity := decimal.Zero
	var cost int64
	value, valued := int64(0), true
	result, resulted := int64(0), true
	for _, hd := range holdings {
		pos := hd.Position
		if pos.Currency != currency {
			return apitypes.InstrumentHoldingsTotal{}, false, nil
		}
		q, err := decimal.NewFromString(pos.Quantity)
		if err != nil {
			return apitypes.InstrumentHoldingsTotal{}, false, err
		}
		quantity = quantity.Add(q)
		if cost, err = money.Add(cost, pos.CostMinor); err != nil {
			return apitypes.InstrumentHoldingsTotal{}, false, err
		}
		v, vErr := pos.MarketValueMinor.Get()
		c, _ := pos.MarketValueCurrency.Get()
		if vErr != nil || c != currency {
			valued = false
		} else if valued {
			if value, err = money.Add(value, v); err != nil {
				return apitypes.InstrumentHoldingsTotal{}, false, err
			}
		}
		t, tErr := pos.TotalMinor.Get()
		if tErr != nil {
			resulted = false
		} else if resulted {
			if result, err = money.Add(result, t); err != nil {
				return apitypes.InstrumentHoldingsTotal{}, false, err
			}
		}
	}
	out := apitypes.InstrumentHoldingsTotal{
		Currency: currency, Quantity: quantity.String(), CostMinor: cost,
		MarketValueMinor: nullable.NewNullNullable[int64](), TotalMinor: nullable.NewNullNullable[int64](),
	}
	if valued {
		out.MarketValueMinor = nullable.NewNullableWithValue(value)
	}
	if resulted {
		out.TotalMinor = nullable.NewNullableWithValue(result)
	}
	return out, true, nil
}

// handlePrices answers the paper's daily prices from `from` to today.
func (h *Handler) handlePrices(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInstrumentID(w, r)
	if !ok {
		return
	}
	today := dates.LatestRecordable()
	from, err := time.Parse(time.DateOnly, r.URL.Query().Get("from"))
	if err != nil || from.After(today) || from.Before(today.AddDate(-maxPriceYears, 0, 0)) {
		httpjson.Error(w, http.StatusBadRequest, "from must be a YYYY-MM-DD date, not in the future and at most ten years back")
		return
	}
	papers, err := h.instruments.ByIDs(r.Context(), []uuid.UUID{id})
	if err != nil {
		family.WriteError(w, err)
		return
	}
	if _, ok := papers[id]; !ok {
		httpjson.Error(w, http.StatusNotFound, "not found")
		return
	}
	series, ok := h.quotes.(priceSeries)
	if !ok {
		family.WriteError(w, errors.New("portfolio: the quote store keeps no price series"))
		return
	}
	quotes, err := series.PriceSeries(r.Context(), id, from, today)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := make([]apitypes.DayPrice, 0, len(quotes))
	for _, q := range quotes {
		out = append(out, apitypes.DayPrice{
			On: q.On.Format(time.DateOnly), Price: q.Price.String(), Currency: q.Currency, Source: q.Source,
		})
	}
	httpjson.Write(w, http.StatusOK, out)
}
