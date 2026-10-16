package account

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
)

// keyRates is what the rates' answer needs of the market data: the key rate
// in force on a day.
type keyRates interface {
	KeyRateOn(ctx context.Context, day time.Time) (decimal.Decimal, bool, error)
}

// WithKeyRates lets the rates' answer strike a rate tied to the key rate.
func (h *Handler) WithKeyRates(k keyRates) *Handler {
	h.keys = k
	return h
}

func (h *Handler) handleRates(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathAccountID(w, r)
	if !ok {
		return
	}
	if _, err := h.store.ByID(r.Context(), p.SpaceID, id); err != nil {
		family.WriteError(w, err)
		return
	}
	h.writeRates(w, r, p.SpaceID, id)
}

func (h *Handler) handleSetRate(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathAccountID(w, r)
	if !ok {
		return
	}
	var req apitypes.AccountRateInput
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	from, err := time.Parse(time.DateOnly, req.FromOn)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "from_on must be YYYY-MM-DD")
		return
	}
	pct, err := decimal.NewFromString(req.Percent)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "percent must be a decimal")
		return
	}
	if err := h.store.SetRate(r.Context(), p.SpaceID, id, Rate{From: from, Kind: RateKind(req.Kind), Percent: pct, ByHand: true}); err != nil {
		family.WriteError(w, err)
		return
	}
	h.writeRates(w, r, p.SpaceID, id)
}

func (h *Handler) handleDeleteRate(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathAccountID(w, r)
	if !ok {
		return
	}
	from, err := time.Parse(time.DateOnly, r.PathValue("fromOn"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "fromOn must be YYYY-MM-DD")
		return
	}
	if err := h.store.DeleteRate(r.Context(), p.SpaceID, id, from); err != nil {
		family.WriteError(w, err)
		return
	}
	h.writeRates(w, r, p.SpaceID, id)
}

// writeRates answers with the account's rates and the one in force today.
func (h *Handler) writeRates(w http.ResponseWriter, r *http.Request, spaceID, id uuid.UUID) {
	list, err := h.store.Rates(r.Context(), spaceID, id)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := apitypes.AccountRates{Rates: RatesAPI(list), Current: nullable.NewNullNullable[apitypes.AccountRateNow](), KeyRate: nullable.NewNullNullable[string]()}
	keyOn := func(day time.Time) (decimal.Decimal, bool, error) {
		if h.keys == nil {
			return decimal.Zero, false, nil
		}
		return h.keys.KeyRateOn(r.Context(), day)
	}
	today := time.Now().UTC()
	if key, ok, err := keyOn(today); err == nil && ok {
		out.KeyRate = nullable.NewNullableWithValue(key.String())
	}
	pct, row, ok, err := RateOn(list, today, keyOn)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	if ok {
		out.Current = nullable.NewNullableWithValue(apitypes.AccountRateNow{
			Percent: pct.StringFixed(3), FromOn: row.From.Format(time.DateOnly), Kind: apitypes.AccountRateNowKind(row.Kind), ByHand: row.ByHand,
		})
	}
	httpjson.Write(w, http.StatusOK, out)
}

// RatesAPI are rates as the API writes them, oldest first.
func RatesAPI(list []Rate) []apitypes.AccountRate {
	out := make([]apitypes.AccountRate, 0, len(list))
	for _, r := range list {
		out = append(out, apitypes.AccountRate{FromOn: r.From.Format(time.DateOnly), Kind: apitypes.AccountRateKind(r.Kind), Percent: r.Percent.StringFixed(3), ByHand: r.ByHand})
	}
	return out
}
