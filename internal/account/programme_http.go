package account

import (
	"net/http"
	"time"

	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/deposit"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/intermediary"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
)

func (h *Handler) handleProgrammes(w http.ResponseWriter, _ *http.Request) {
	all, err := deposit.Catalog()
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "the catalog of programmes does not read")
		return
	}
	banks, _ := intermediary.Load()
	out := apitypes.DepositProgrammes{Programmes: make([]apitypes.DepositProgramme, 0, len(all))}
	for _, p := range all {
		e := p.Editions[len(p.Editions)-1]
		bank, _ := banks.ByID(p.Bank)
		dp := apitypes.DepositProgramme{
			Id: p.ID, BankId: p.Bank, BankName: bank.Name, Name: p.Name, Kind: apitypes.DepositProgrammeKind(p.Kind),
			EditionFrom: e.From, CheckedOn: e.CheckedOn, Sources: e.Sources, Notes: e.Notes, Variants: []apitypes.DepositVariant{},
		}
		for _, v := range e.Variants {
			dp.Variants = append(dp.Variants, variantAPI(v))
		}
		out.Programmes = append(out.Programmes, dp)
	}
	httpjson.Write(w, http.StatusOK, out)
}

// variantAPI is a variant as the catalog's listing writes it: the terms and
// least sum the bank has a rate for, the conditions.
func variantAPI(v deposit.Variant) apitypes.DepositVariant {
	out := apitypes.DepositVariant{
		Id: v.ID, Name: v.Name, Currency: v.Money(), Ops: apitypes.DepositVariantOps(v.Ops), Terms: []apitypes.TermRange{},
		Accrual: nullable.NewNullNullable[apitypes.DepositVariantAccrual](), TermUnit: nullable.NewNullNullable[apitypes.DepositVariantTermUnit](),
		Welcome: nullable.NewNullNullable[string](), Conditions: conditionsAPI(v, nil, nil, time.Time{}),
	}
	if v.Accrual != "" {
		out.Accrual = nullable.NewNullableWithValue(apitypes.DepositVariantAccrual(v.Accrual))
	}
	if v.TermUnit != "" {
		out.TermUnit = nullable.NewNullableWithValue(apitypes.DepositVariantTermUnit(v.TermUnit))
	}
	for _, p := range v.Payouts {
		out.Payouts = append(out.Payouts, apitypes.DepositVariantPayouts(p))
	}
	out.Audience = make([]apitypes.DepositVariantAudience, 0, len(v.Audience))
	for _, a := range v.Audience {
		out.Audience = append(out.Audience, apitypes.DepositVariantAudience(a))
	}
	least := int64(-1)
	seen := map[[2]int]bool{}
	for _, c := range v.Grid {
		if least < 0 || c.AmountFrom < least {
			least = c.AmountFrom
		}
		if c.From > 0 && !seen[[2]int{c.From, c.To}] {
			seen[[2]int{c.From, c.To}] = true
			out.Terms = append(out.Terms, apitypes.TermRange{From: c.From, To: c.To})
		}
	}
	out.AmountFromMinor = max(least, 0)
	if len(v.Schedule) > 0 {
		out.Welcome = nullable.NewNullableWithValue(v.Schedule[0].Name)
	}
	return out
}

// conditionsAPI are the variant's conditions — its periods' requirements
// and its bonuses — with how tell says they stand in month (nothing met
// without tell). With a deposit's term — only the bonuses of the term, at
// its plus.
func conditionsAPI(v deposit.Variant, term *int, tell func(id string, month time.Time) Told, month time.Time) []apitypes.ProgrammeCondition {
	out := []apitypes.ProgrammeCondition{}
	state := func(c *apitypes.ProgrammeCondition) {
		c.Told, c.MeasuredMinor, c.MeasuredMonth = apitypes.ProgrammeConditionToldNone, nullable.NewNullNullable[int64](), nullable.NewNullNullable[string]()
		if tell == nil {
			return
		}
		t := tell(c.Id, month)
		c.Met, c.Level, c.Told = t.Met, t.Level, apitypes.ProgrammeConditionTold(t.By)
		if t.By == "data" {
			c.MeasuredMinor = nullable.NewNullableWithValue(t.Value)
		}
		if !t.Month.IsZero() {
			c.MeasuredMonth = nullable.NewNullableWithValue(t.Month.Format("2006-01"))
		}
	}
	seen := map[string]bool{}
	for _, s := range v.Schedule {
		if s.Requires == "" || seen[s.Requires] {
			continue
		}
		seen[s.Requires] = true
		c := apitypes.ProgrammeCondition{
			Id: s.Requires, Name: deposit.Requirements[s.Requires], Kind: apitypes.Requirement, Check: apitypes.ProgrammeConditionCheckManual,
			Group: nullable.NewNullNullable[string](), Plus: nullable.NewNullNullable[string](), Levels: []apitypes.BonusLevel{},
		}
		state(&c)
		out = append(out, c)
	}
	for _, b := range v.Bonuses {
		plus := b.Plus
		if term != nil {
			given, ok := b.PlusFor(0, *term)
			if !ok {
				continue
			}
			if len(b.Levels) == 0 {
				plus = given
			}
		}
		c := apitypes.ProgrammeCondition{
			Id: b.ID, Name: b.Name, Kind: apitypes.Bonus, Check: apitypes.ProgrammeConditionCheck(b.Check),
			Group: nullable.NewNullNullable[string](), Plus: nullable.NewNullNullable[string](), Levels: []apitypes.BonusLevel{},
		}
		if b.Group != "" {
			c.Group = nullable.NewNullableWithValue(b.Group)
		}
		if plus != "" {
			c.Plus = nullable.NewNullableWithValue(plus)
		}
		for _, l := range b.Levels {
			c.Levels = append(c.Levels, apitypes.BonusLevel{From: l.From, Plus: l.Plus})
		}
		state(&c)
		out = append(out, c)
	}
	return out
}

func (h *Handler) handleSetProgramme(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathAccountID(w, r)
	if !ok {
		return
	}
	var req apitypes.AccountProgrammeInput
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	opened, err := time.Parse(time.DateOnly, req.OpenedOn)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "opened_on must be YYYY-MM-DD")
		return
	}
	b := Binding{Programme: req.ProgrammeId, Variant: req.VariantId, OpenedOn: opened, Conditions: ConditionsMode(req.Conditions)}
	if t, err := req.Term.Get(); err == nil {
		b.Term = &t
	}
	if a, err := req.AmountMinor.Get(); err == nil {
		b.Amount = &a
	}
	if way, err := req.Payout.Get(); err == nil {
		w := deposit.Payout(way)
		b.Payout = &w
	}
	if err := h.store.SetBinding(r.Context(), p.SpaceID, id, b); err != nil {
		family.WriteError(w, err)
		return
	}
	h.writeRates(w, r, p.SpaceID, id)
}

func (h *Handler) handleDeleteProgramme(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathAccountID(w, r)
	if !ok {
		return
	}
	if err := h.store.DeleteBinding(r.Context(), p.SpaceID, id); err != nil {
		family.WriteError(w, err)
		return
	}
	h.writeRates(w, r, p.SpaceID, id)
}

func (h *Handler) handleSetMark(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathAccountID(w, r)
	if !ok {
		return
	}
	var req apitypes.ConditionMarkInput
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	from, err := time.Parse(time.DateOnly, req.FromOn)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "from_on must be YYYY-MM-DD")
		return
	}
	if err := h.store.SetMark(r.Context(), p.SpaceID, id, Mark{Condition: req.Condition, From: from, Level: req.Level, Met: req.Met}); err != nil {
		family.WriteError(w, err)
		return
	}
	h.writeRates(w, r, p.SpaceID, id)
}

func (h *Handler) handleDeleteMark(w http.ResponseWriter, r *http.Request) {
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
	if err := h.store.DeleteMark(r.Context(), p.SpaceID, id, r.PathValue("condition"), from); err != nil {
		family.WriteError(w, err)
		return
	}
	h.writeRates(w, r, p.SpaceID, id)
}

// MarksAPI are marks as the API writes them.
func MarksAPI(list []Mark) []apitypes.ConditionMark {
	out := make([]apitypes.ConditionMark, 0, len(list))
	for _, m := range list {
		out = append(out, apitypes.ConditionMark{Condition: m.Condition, FromOn: m.From.Format(time.DateOnly), Level: m.Level, Met: m.Met})
	}
	return out
}

// BindingAPI is a programme as the export writes it, nil without one.
func BindingAPI(b *Binding) nullable.Nullable[apitypes.AccountProgrammeInput] {
	if b == nil {
		return nullable.NewNullNullable[apitypes.AccountProgrammeInput]()
	}
	out := apitypes.AccountProgrammeInput{
		ProgrammeId: b.Programme, VariantId: b.Variant, OpenedOn: b.OpenedOn.Format(time.DateOnly),
		Conditions: apitypes.AccountProgrammeInputConditions(b.Conditions),
		Term:       nullable.NewNullNullable[int](), AmountMinor: nullable.NewNullNullable[int64](),
	}
	if b.Term != nil {
		out.Term = nullable.NewNullableWithValue(*b.Term)
	}
	if b.Amount != nil {
		out.AmountMinor = nullable.NewNullableWithValue(*b.Amount)
	}
	if b.Payout != nil {
		out.Payout = nullable.NewNullableWithValue(apitypes.AccountProgrammeInputPayout(*b.Payout))
	}
	return nullable.NewNullableWithValue(out)
}
