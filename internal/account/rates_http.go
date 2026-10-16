package account

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/deposit"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/intermediary"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
)

// keyRates is what the rates' answer needs of the market data: the key rate
// in force on a day.
type keyRates interface {
	KeyRateOn(ctx context.Context, day time.Time) (decimal.Decimal, bool, error)
}

// MeasureSource is what tells a programme's conditions from the family's
// data in the automatic mode (Р-53).
type MeasureSource interface {
	MeasuresFor(ctx context.Context, spaceID uuid.UUID) (Measure, error)
}

// WithMeasures lets the rates' answer tell the conditions of an account in
// the automatic mode.
func (h *Handler) WithMeasures(m MeasureSource) *Handler {
	h.measures = m
	return h
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

// writeRates answers with the account's rates, its programme, and the rate
// in force today and where it comes from.
func (h *Handler) writeRates(w http.ResponseWriter, r *http.Request, spaceID, id uuid.UUID) {
	ctx := r.Context()
	a, err := h.store.ByID(ctx, spaceID, id)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	list, err := h.store.Rates(ctx, spaceID, id)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	binding, err := h.store.Binding(ctx, spaceID, id)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	marks, err := h.store.Marks(ctx, spaceID, id)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := apitypes.AccountRates{
		Rates: RatesAPI(list), Current: nullable.NewNullNullable[apitypes.AccountRateNow](), KeyRate: nullable.NewNullNullable[string](),
		Source: apitypes.AccountRatesSourceNone, Programme: nullable.NewNullNullable[apitypes.AccountProgramme](), Quote: nullable.NewNullNullable[apitypes.ProgrammeQuote](),
		Conditions: []apitypes.ProgrammeCondition{}, Marks: MarksAPI(marks),
	}
	keyOn := func(day time.Time) (decimal.Decimal, bool, error) {
		if h.keys == nil {
			return decimal.Zero, false, nil
		}
		return h.keys.KeyRateOn(ctx, day)
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if key, ok, err := keyOn(today); err == nil && ok {
		out.KeyRate = nullable.NewNullableWithValue(key.String())
	}
	pct, row, ok, err := RateOn(list, today, keyOn)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	if ok {
		out.Source = apitypes.AccountRatesSourceByHand
		out.Current = nullable.NewNullableWithValue(apitypes.AccountRateNow{
			Percent: pct.StringFixed(3), FromOn: row.From.Format(time.DateOnly), Kind: apitypes.AccountRateNowKind(row.Kind), ByHand: row.ByHand,
		})
	}
	if binding != nil {
		if p, found := deposit.ByID(binding.Programme); found {
			if err := h.writeProgramme(ctx, &out, *binding, p, marks, a, today, keyOn); err != nil {
				family.WriteError(w, err)
				return
			}
		}
	}
	httpjson.Write(w, http.StatusOK, out)
}

// writeProgramme fills the answer's programme, its quote on today's balance
// and the conditions with what the marks say of them this month.
func (h *Handler) writeProgramme(ctx context.Context, out *apitypes.AccountRates, b Binding, p deposit.Programme, marks []Mark, a WithBalance, today time.Time, keyOn func(time.Time) (decimal.Decimal, bool, error)) error {
	banks, _ := intermediary.Load()
	bank, _ := banks.ByID(p.Bank)
	on := today
	if p.Kind == deposit.KindDeposit {
		on = b.OpenedOn
	}
	e, ok := p.EditionOn(on)
	if !ok {
		e = p.Editions[len(p.Editions)-1]
	}
	v, _ := e.Variant(b.Variant)
	ap := apitypes.AccountProgramme{
		ProgrammeId: p.ID, ProgrammeName: p.Name, BankId: p.Bank, BankName: bank.Name, Kind: apitypes.AccountProgrammeKind(p.Kind),
		VariantId: b.Variant, VariantName: v.Name, OpenedOn: b.OpenedOn.Format(time.DateOnly), Conditions: apitypes.AccountProgrammeConditions(b.Conditions),
		Term: nullable.NewNullNullable[int](), TermUnit: nullable.NewNullNullable[apitypes.AccountProgrammeTermUnit](), AmountMinor: nullable.NewNullNullable[int64](),
	}
	if b.Term != nil {
		ap.Term = nullable.NewNullableWithValue(*b.Term)
		ap.TermUnit = nullable.NewNullableWithValue(apitypes.AccountProgrammeTermUnit(v.TermUnit))
	}
	if b.Amount != nil {
		ap.AmountMinor = nullable.NewNullableWithValue(*b.Amount)
	}
	out.Programme = nullable.NewNullableWithValue(ap)
	var measure Measure
	if b.Conditions == ConditionsAuto && h.measures != nil {
		m, err := h.measures.MeasuresFor(ctx, a.SpaceID)
		if err != nil {
			return err
		}
		measure = m
	}
	out.Conditions = conditionsAPI(v, Tell(b, p, marks, measure, today), time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC))
	balance := int64(0)
	if a.Balance != nil {
		balance = a.Balance.AmountMinor
	}
	res, ok, err := Resolve(RateSource{Binding: &b, Marks: marks, Measure: measure}, today, balance, keyOn)
	if err != nil || !ok {
		return err
	}
	q := *res.Quote
	pq := apitypes.ProgrammeQuote{
		Percent: q.Percent.StringFixed(3), EditionFrom: q.Edition, CheckedOn: e.CheckedOn, Sources: e.Sources, Notes: e.Notes,
		Period: nullable.NewNullNullable[string](), Bonuses: q.Bonuses, Parts: []apitypes.QuotePart{}, Mgs: nullable.NewNullNullable[string](),
	}
	if pq.Bonuses == nil {
		pq.Bonuses = []string{}
	}
	if q.Period != "" {
		pq.Period = nullable.NewNullableWithValue(q.Period)
	}
	if v.MGS != "" {
		pq.Mgs = nullable.NewNullableWithValue(v.MGS)
	}
	for _, part := range q.Parts {
		pq.Parts = append(pq.Parts, apitypes.QuotePart{FromMinor: part.From, ToMinor: part.To, Percent: part.Percent.StringFixed(3)})
	}
	out.Quote = nullable.NewNullableWithValue(pq)
	if out.Source == apitypes.AccountRatesSourceNone {
		out.Source = apitypes.AccountRatesSourceProgramme
	}
	return nil
}

// RatesAPI are rates as the API writes them, oldest first.
func RatesAPI(list []Rate) []apitypes.AccountRate {
	out := make([]apitypes.AccountRate, 0, len(list))
	for _, r := range list {
		out = append(out, apitypes.AccountRate{FromOn: r.From.Format(time.DateOnly), Kind: apitypes.AccountRateKind(r.Kind), Percent: r.Percent.StringFixed(3), ByHand: r.ByHand})
	}
	return out
}
