package valuables

import (
	"errors"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler serves a valuables account's storage, pieces and bank prices.
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
	srv.Mount("GET /api/v1/accounts/{accountId}/valuables", view(h.handleGet))
	srv.Mount("PUT /api/v1/accounts/{accountId}/valuables/storage", edit(h.handleStorage))
	srv.Mount("POST /api/v1/accounts/{accountId}/valuables/items", edit(h.handleAdd))
	srv.Mount("PUT /api/v1/accounts/{accountId}/valuables/items/{itemId}", edit(h.handleUpdate))
	srv.Mount("DELETE /api/v1/accounts/{accountId}/valuables/items/{itemId}", edit(h.handleDelete))
	srv.Mount("PUT /api/v1/accounts/{accountId}/valuables/bank-prices", edit(h.handleBankPrice))
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, name+" must be a uuid")
		return uuid.UUID{}, false
	}
	return id, true
}

// write answers with the storage, or with the error: a piece not found is
// a 404 like an account not found.
func write(w http.ResponseWriter, h Holding, err error) {
	if errors.Is(err, ErrNotFound) {
		httpjson.Error(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, HoldingAPI(h))
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathID(w, r, "accountId")
	if !ok {
		return
	}
	got, err := h.svc.Of(r.Context(), p.SpaceID, id)
	write(w, got, err)
}

func (h *Handler) handleStorage(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathID(w, r, "accountId")
	if !ok {
		return
	}
	var req apitypes.ValuablesStorage
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	st := Storage{Place: Place(req.Place), Bank: req.Bank}
	if req.Until.IsSpecified() && !req.Until.IsNull() {
		d, err := time.Parse(time.DateOnly, req.Until.MustGet())
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "until must be YYYY-MM-DD")
			return
		}
		st.Until = &d
	}
	if req.FeeMinor.IsSpecified() && !req.FeeMinor.IsNull() {
		v := req.FeeMinor.MustGet()
		st.FeeMinor = &v
	}
	if req.FeePeriod.IsSpecified() && !req.FeePeriod.IsNull() {
		v := FeePeriod(req.FeePeriod.MustGet())
		st.FeePeriod = &v
	}
	if req.BuybackPercent.IsSpecified() && !req.BuybackPercent.IsNull() {
		v, err := decimal.NewFromString(req.BuybackPercent.MustGet())
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "buyback_percent must be a decimal")
			return
		}
		st.Buyback = &v
	}
	got, err := h.svc.SetStorage(r.Context(), p.SpaceID, id, st)
	write(w, got, err)
}

// itemOf reads a piece from its request; false after answering 400.
func itemOf(w http.ResponseWriter, req apitypes.ValuablesItemInput) (Item, bool) {
	weight, err := decimal.NewFromString(req.Weight)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "weight must be a decimal")
		return Item{}, false
	}
	fineness, err := decimal.NewFromString(req.Fineness)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "fineness must be a decimal")
		return Item{}, false
	}
	bought, err := time.Parse(time.DateOnly, req.BoughtOn)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "bought_on must be YYYY-MM-DD")
		return Item{}, false
	}
	it := Item{
		Kind: Kind(req.Kind), Metal: marketdata.Metal(req.Metal), Weight: weight, Fineness: fineness, Maker: req.Maker,
		ForeignMade: req.ForeignMade, Serial: req.Serial, Certificate: req.Certificate, Sealed: req.Sealed,
		BoughtOn: bought, CostMinor: req.CostMinor, Currency: req.Currency, Note: req.Note,
	}
	if req.OwnerUserId.IsSpecified() && !req.OwnerUserId.IsNull() {
		v := req.OwnerUserId.MustGet()
		it.OwnerUserID = &v
	}
	return it, true
}

func (h *Handler) handleAdd(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathID(w, r, "accountId")
	if !ok {
		return
	}
	var req apitypes.ValuablesItemInput
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	it, ok := itemOf(w, req)
	if !ok {
		return
	}
	got, err := h.svc.AddItem(r.Context(), p.SpaceID, id, it)
	write(w, got, err)
}

func (h *Handler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathID(w, r, "accountId")
	if !ok {
		return
	}
	itemID, ok := pathID(w, r, "itemId")
	if !ok {
		return
	}
	var req apitypes.ValuablesItemInput
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	it, ok := itemOf(w, req)
	if !ok {
		return
	}
	got, err := h.svc.UpdateItem(r.Context(), p.SpaceID, id, itemID, it)
	write(w, got, err)
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathID(w, r, "accountId")
	if !ok {
		return
	}
	itemID, ok := pathID(w, r, "itemId")
	if !ok {
		return
	}
	got, err := h.svc.DeleteItem(r.Context(), p.SpaceID, id, itemID)
	write(w, got, err)
}

func (h *Handler) handleBankPrice(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathID(w, r, "accountId")
	if !ok {
		return
	}
	var req apitypes.ValuablesBankPrice
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	price, err := decimal.NewFromString(req.Price)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "price must be a decimal")
		return
	}
	on, err := time.Parse(time.DateOnly, req.On)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "on must be YYYY-MM-DD")
		return
	}
	got, err := h.svc.SetBankPrice(r.Context(), p.SpaceID, id, BankPrice{Metal: marketdata.Metal(req.Metal), Price: price, On: on})
	write(w, got, err)
}

func dateOf(t *time.Time) nullable.Nullable[string] {
	if t == nil {
		return nullable.NewNullNullable[string]()
	}
	return nullable.NewNullableWithValue(t.Format(time.DateOnly))
}

func minorOf(v *int64) nullable.Nullable[int64] {
	if v == nil {
		return nullable.NewNullNullable[int64]()
	}
	return nullable.NewNullableWithValue(*v)
}

// HoldingAPI is a storage as the API writes it.
func HoldingAPI(h Holding) apitypes.Valuables {
	st := apitypes.ValuablesStorage{
		Place: apitypes.ValuablesStoragePlace(h.Storage.Place), Bank: h.Storage.Bank, Until: dateOf(h.Storage.Until),
		FeeMinor: minorOf(h.Storage.FeeMinor), FeePeriod: nullable.NewNullNullable[apitypes.ValuablesStorageFeePeriod](),
		BuybackPercent: nullable.NewNullNullable[string](),
	}
	if h.Storage.FeePeriod != nil {
		st.FeePeriod = nullable.NewNullableWithValue(apitypes.ValuablesStorageFeePeriod(*h.Storage.FeePeriod))
	}
	if h.Storage.Buyback != nil {
		st.BuybackPercent = nullable.NewNullableWithValue(h.Storage.Buyback.StringFixed(2))
	}
	out := apitypes.Valuables{
		Storage: st, Items: make([]apitypes.ValuablesItem, 0, len(h.Items)), BankPrices: make([]apitypes.ValuablesBankPrice, 0, len(h.BankPrices)),
		MarketMinor: minorOf(h.Market), SellMinor: minorOf(h.Sell), Currency: "RUB",
	}
	for _, b := range h.BankPrices {
		out.BankPrices = append(out.BankPrices, apitypes.ValuablesBankPrice{Metal: apitypes.Metal(b.Metal), Price: b.Price.String(), On: b.On.Format(time.DateOnly)})
	}
	for _, v := range h.Items {
		it := apitypes.ValuablesItem{
			Id: v.ID, Kind: apitypes.ValuablesItemKind(v.Kind), Metal: apitypes.Metal(v.Metal), Weight: v.Weight.String(),
			Fineness: v.Fineness.StringFixed(1), Maker: v.Maker, ForeignMade: v.ForeignMade, Serial: v.Serial,
			Certificate: v.Certificate, Sealed: v.Sealed, BoughtOn: v.BoughtOn.Format(time.DateOnly), CostMinor: v.CostMinor,
			Currency: v.Currency, OwnerUserId: nullable.NewNullNullable[uuid.UUID](), Note: v.Note,
			PureGrams: v.Pure().Round(4).String(), MarketMinor: minorOf(v.Value.Market), SellMinor: minorOf(v.Value.Sell),
			SellBy: apitypes.ValuablesItemSellBy(v.Value.By), BuybackPercent: nullable.NewNullNullable[string](),
			BankPriceOn: dateOf(v.Value.BankOn), PriceOn: dateOf(v.Value.PriceOn),
			TaxFreeFrom: v.TaxFreeFrom().Format(time.DateOnly), HardToSell: []apitypes.ValuablesItemHardToSell{},
		}
		if v.OwnerUserID != nil {
			it.OwnerUserId = nullable.NewNullableWithValue(*v.OwnerUserID)
		}
		if v.Value.Percent != nil {
			it.BuybackPercent = nullable.NewNullableWithValue(v.Value.Percent.StringFixed(2))
		}
		for _, hard := range v.HardToSell() {
			it.HardToSell = append(it.HardToSell, apitypes.ValuablesItemHardToSell(hard))
		}
		out.Items = append(out.Items, it)
	}
	return out
}
