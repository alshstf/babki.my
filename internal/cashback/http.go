package cashback

import (
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/intermediary"
	"babki.my/babki/internal/loyalty"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler serves a card's loyalty programme and the catalog of programmes.
type Handler struct {
	svc  *Service
	auth *family.Auth
	sm   *scs.SessionManager
}

func NewHandler(svc *Service, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{svc: svc, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	view := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleViewer, fn)))
	}
	edit := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleEditor, fn)))
	}
	srv.Mount("GET /api/v1/loyalty-programmes", view(h.handleProgrammes))
	srv.Mount("GET /api/v1/accounts/{accountId}/cashback", view(h.handleCard))
	srv.Mount("PUT /api/v1/accounts/{accountId}/cashback/programme", edit(h.handleBind))
	srv.Mount("DELETE /api/v1/accounts/{accountId}/cashback/programme", edit(h.handleUnbind))
	srv.Mount("PUT /api/v1/accounts/{accountId}/cashback/picks", edit(h.handlePicks))
}

func accountID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("accountId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid accountId")
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) handleProgrammes(w http.ResponseWriter, _ *http.Request) {
	all, err := loyalty.Catalog()
	if err != nil {
		family.WriteError(w, err)
		return
	}
	banks, _ := intermediary.Load()
	out := make([]apitypes.LoyaltyProgramme, 0, len(all))
	for _, p := range all {
		e := p.Editions[len(p.Editions)-1]
		bank, _ := banks.ByID(p.Bank)
		out = append(out, apitypes.LoyaltyProgramme{
			Id: p.ID, BankId: p.Bank, BankName: bank.Name, Name: p.Name, EditionFrom: e.From,
			Period: apitypes.LoyaltyPeriod(e.Period), Variants: variantsAPI(e.Variants), Categories: categoriesAPI(e.Categories),
			Sources: e.Sources, Notes: e.Notes,
		})
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (h *Handler) handleCard(w http.ResponseWriter, r *http.Request) {
	day := time.Now().UTC()
	if raw := r.URL.Query().Get("on"); raw != "" {
		d, err := time.Parse(time.DateOnly, raw)
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "on must be YYYY-MM-DD")
			return
		}
		day = d
	}
	h.writeCard(w, r, day)
}

func (h *Handler) writeCard(w http.ResponseWriter, r *http.Request, day time.Time) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := accountID(w, r)
	if !ok {
		return
	}
	c, err := h.svc.CardOf(r.Context(), p.SpaceID, id, day)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, cardAPI(c))
}

func (h *Handler) handleBind(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := accountID(w, r)
	if !ok {
		return
	}
	var req apitypes.CardLoyaltyInput
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	if err := h.svc.Bind(r.Context(), p.SpaceID, id, Binding{Programme: req.ProgrammeId, Variant: req.VariantId}); err != nil {
		family.WriteError(w, err)
		return
	}
	h.writeCard(w, r, time.Now().UTC())
}

func (h *Handler) handleUnbind(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := accountID(w, r)
	if !ok {
		return
	}
	if err := h.svc.Unbind(r.Context(), p.SpaceID, id); err != nil {
		family.WriteError(w, err)
		return
	}
	h.writeCard(w, r, time.Now().UTC())
}

func (h *Handler) handlePicks(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := accountID(w, r)
	if !ok {
		return
	}
	var req apitypes.CardPicksInput
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	day, err := time.Parse(time.DateOnly, req.On)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "on must be YYYY-MM-DD")
		return
	}
	picks := make([]Pick, 0, len(req.Picks))
	for _, pk := range req.Picks {
		pct, err := decimal.NewFromString(pk.Percent)
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "percent must be a decimal")
			return
		}
		picks = append(picks, Pick{Category: pk.CategoryId, Percent: pct})
	}
	if err := h.svc.SetPicks(r.Context(), p.SpaceID, id, day, picks); err != nil {
		family.WriteError(w, err)
		return
	}
	h.writeCard(w, r, day)
}

func variantsAPI(list []loyalty.Variant) []apitypes.LoyaltyVariant {
	out := make([]apitypes.LoyaltyVariant, 0, len(list))
	for _, v := range list {
		out = append(out, apitypes.LoyaltyVariant{Id: v.ID, Name: v.Name, Pick: v.Pick, Of: v.Of, CapMinor: v.Cap})
	}
	return out
}

func categoriesAPI(list []loyalty.Category) []apitypes.LoyaltyCategory {
	out := make([]apitypes.LoyaltyCategory, 0, len(list))
	for _, c := range list {
		out = append(out, apitypes.LoyaltyCategory{Id: c.ID, Name: c.Name, ByMerchant: c.ByMerchant})
	}
	return out
}

func cardAPI(c Card) apitypes.CardCashback {
	out := apitypes.CardCashback{
		Programme: nullable.NewNullNullable[apitypes.CardLoyalty](), PeriodFrom: nullable.NewNullNullable[string](), PeriodTo: nullable.NewNullNullable[string](),
		Lines: []apitypes.CardCashbackLine{}, BonusMinor: c.Bonus, Capped: c.Capped, UncodedMinor: c.Uncoded, UncodedCount: c.Uncount,
	}
	if c.Binding == nil {
		return out
	}
	p, _ := loyalty.ByID(c.Binding.Programme)
	banks, _ := intermediary.Load()
	bank, _ := banks.ByID(p.Bank)
	cl := apitypes.CardLoyalty{
		ProgrammeId: c.Binding.Programme, ProgrammeName: c.Name, BankName: bank.Name, VariantId: c.Binding.Variant,
		VariantName: c.Variant.Name, Pick: c.Variant.Pick, Of: c.Variant.Of, CapMinor: c.Variant.Cap, Period: apitypes.LoyaltyPeriod(c.Edition.Period),
		EditionFrom: c.Edition.From, Categories: categoriesAPI(c.Edition.Categories), Sources: c.Edition.Sources, Notes: c.Edition.Notes, Known: c.Known,
	}
	if cl.Sources == nil {
		cl.Sources = []string{}
	}
	out.Programme = nullable.NewNullableWithValue(cl)
	if !c.Known {
		return out
	}
	out.PeriodFrom = nullable.NewNullableWithValue(c.From.Format(time.DateOnly))
	out.PeriodTo = nullable.NewNullableWithValue(c.To.Format(time.DateOnly))
	for _, l := range c.Lines {
		out.Lines = append(out.Lines, apitypes.CardCashbackLine{
			CategoryId: l.Category, Name: l.Name, Percent: l.Percent.String(), SpentMinor: l.Spent, BonusMinor: l.Bonus, Purchases: l.Counts,
		})
	}
	return out
}
