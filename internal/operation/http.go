package operation

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// defaultListLimit/maxListLimit bound GET .../operations. A limit above
// maxListLimit is clamped rather than rejected with 400, matching the
// instrument catalog search: pagination is a best-effort detail, not
// something a client should get an error for overshooting.
const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// Handler exposes the operations journal (and transfers) over HTTP.
type Handler struct {
	svc   *Service
	store *Store
	auth  *family.Auth
	sm    *scs.SessionManager
}

func NewHandler(svc *Service, store *Store, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{svc: svc, store: store, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	view := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleViewer, fn)))
	}
	edit := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleEditor, fn)))
	}
	srv.Mount("POST /api/v1/operations", edit(h.handleCreate))
	srv.Mount("GET /api/v1/accounts/{accountId}/operations", view(h.handleListByAccount))
	srv.Mount("DELETE /api/v1/operations/{operationId}", edit(h.handleDelete))
	srv.Mount("POST /api/v1/operations/transfer", edit(h.handleTransfer))
}

// writeError maps operation-specific errors to HTTP responses, falling back
// to family.WriteError for everything else. ErrInconsistent gets its own
// branch (409, with the engine's explanation in the body) since it isn't one
// of the family package's sentinel errors.
func writeError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrInconsistent) {
		httpjson.Error(w, http.StatusConflict, err.Error())
		return
	}
	family.WriteError(w, err)
}

func toAPI(o Operation) apitypes.Operation {
	out := apitypes.Operation{
		Id:          o.ID,
		AccountId:   o.AccountID,
		Type:        apitypes.OperationType(o.Type),
		OccurredOn:  o.OccurredOn.Format("2006-01-02"),
		AmountMinor: o.AmountMinor,
		Currency:    o.Currency,
		FeeMinor:    o.FeeMinor,
		Note:        o.Note,
		Source:      o.Source,
		CreatedAt:   o.CreatedAt,
	}
	if o.InstrumentID != nil {
		out.InstrumentId = nullable.NewNullableWithValue(*o.InstrumentID)
	}
	if o.SettledOn != nil {
		out.SettledOn = nullable.NewNullableWithValue(o.SettledOn.Format("2006-01-02"))
	}
	if o.Quantity != nil {
		out.Quantity = nullable.NewNullableWithValue(o.Quantity.String())
	}
	if o.Price != nil {
		out.Price = nullable.NewNullableWithValue(o.Price.String())
	}
	if o.SplitRatio != nil {
		out.SplitRatio = nullable.NewNullableWithValue(o.SplitRatio.String())
	}
	if o.TransferGroupID != nil {
		out.TransferGroupId = nullable.NewNullableWithValue(*o.TransferGroupID)
	}
	return out
}

// parseDate parses a YYYY-MM-DD date, matching account.parseAsOf's format.
// Business-rule checks (e.g. "not in the future") are left to the service,
// which already enforces them when replaying the journal.
func parseDate(s string) (time.Time, error) {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("must be YYYY-MM-DD")
	}
	return d, nil
}

// parseNullableDecimal converts an optional decimal-as-string field. An
// absent or explicit-null field yields (nil, true). A present-but-garbage
// string writes a 400 response and returns ok=false so the caller can bail
// out before ever reaching the service.
func parseNullableDecimal(w http.ResponseWriter, n nullable.Nullable[string], field string) (*decimal.Decimal, bool) {
	if !n.IsSpecified() || n.IsNull() {
		return nil, true
	}
	d, err := decimal.NewFromString(n.MustGet())
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, field+" must be a decimal string")
		return nil, false
	}
	return &d, true
}

func pathAccountID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("accountId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid accountId")
		return uuid.Nil, false
	}
	return id, true
}

func pathOperationID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("operationId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid operationId")
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.CreateOperationRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}

	occurredOn, err := parseDate(req.OccurredOn)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "occurred_on "+err.Error())
		return
	}

	var settledOn *time.Time
	if req.SettledOn.IsSpecified() && !req.SettledOn.IsNull() {
		t, err := parseDate(req.SettledOn.MustGet())
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "settled_on "+err.Error())
			return
		}
		settledOn = &t
	}

	quantity, ok := parseNullableDecimal(w, req.Quantity, "quantity")
	if !ok {
		return
	}
	price, ok := parseNullableDecimal(w, req.Price, "price")
	if !ok {
		return
	}
	splitRatio, ok := parseNullableDecimal(w, req.SplitRatio, "split_ratio")
	if !ok {
		return
	}

	var instrumentID *uuid.UUID
	if req.InstrumentId.IsSpecified() && !req.InstrumentId.IsNull() {
		v := req.InstrumentId.MustGet()
		instrumentID = &v
	}

	feeMinor := int64(0)
	if req.FeeMinor != nil {
		feeMinor = *req.FeeMinor
	}
	note := ""
	if req.Note != nil {
		note = *req.Note
	}

	op := Operation{
		AccountID:    req.AccountId,
		InstrumentID: instrumentID,
		Type:         Type(req.Type),
		OccurredOn:   occurredOn,
		SettledOn:    settledOn,
		Quantity:     quantity,
		Price:        price,
		AmountMinor:  req.AmountMinor,
		Currency:     req.Currency,
		FeeMinor:     feeMinor,
		Note:         note,
		SplitRatio:   splitRatio,
	}

	created, err := h.svc.Create(r.Context(), p.SpaceID, op)
	if err != nil {
		writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, toAPI(created))
}

func (h *Handler) handleListByAccount(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	accountID, ok := pathAccountID(w, r)
	if !ok {
		return
	}

	limit := defaultListLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			offset = n
		}
	}

	ops, err := h.store.ListByAccount(r.Context(), p.SpaceID, accountID, limit, offset)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := make([]apitypes.Operation, 0, len(ops))
	for _, o := range ops {
		out = append(out, toAPI(o))
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathOperationID(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), p.SpaceID, id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleTransfer(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.TransferRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}

	occurredOn, err := parseDate(req.OccurredOn)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "occurred_on "+err.Error())
		return
	}
	quantity, err := decimal.NewFromString(req.Quantity)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "quantity must be a decimal string")
		return
	}

	var costOverride *int64
	if req.CostMinor.IsSpecified() && !req.CostMinor.IsNull() {
		v := req.CostMinor.MustGet()
		costOverride = &v
	}
	note := ""
	if req.Note != nil {
		note = *req.Note
	}

	out, in, err := h.svc.CreateTransfer(r.Context(), p.SpaceID, TransferParams{
		FromAccountID:     req.FromAccountId,
		ToAccountID:       req.ToAccountId,
		InstrumentID:      req.InstrumentId,
		Quantity:          quantity,
		OccurredOn:        occurredOn,
		CostMinorOverride: costOverride,
		Note:              note,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, apitypes.TransferResponse{Out: toAPI(out), In: toAPI(in)})
}
