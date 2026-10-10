package receipt

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler serves the receipts.
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
	srv.Mount("GET /api/v1/receipts/match", view(h.handleMatch))
	srv.Mount("POST /api/v1/receipts", edit(h.handleCreate))
	srv.Mount("GET /api/v1/receipts", view(h.handleList))
	srv.Mount("POST /api/v1/receipts/import", edit(h.handleImport))
	srv.Mount("POST /api/v1/receipts/{receiptId}/split", edit(h.handleResplit))
	srv.Mount("PUT /api/v1/receipts/{receiptId}/operation", edit(h.handleAttach))
	// «Поделиться» on a phone is a navigation: one not signed in goes to the
	// sign-in page rather than to an error.
	srv.Mount("POST /share", h.sm.LoadAndSave(toSignIn(h.auth.RequireAuth(family.RequireRole(family.RoleEditor, http.HandlerFunc(h.handleShare))))))
}

func (h *Handler) handleMatch(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	q := r.URL.Query()
	total, err := strconv.ParseInt(q.Get("total_minor"), 10, 64)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "total_minor must be a whole number")
		return
	}
	at, err := time.Parse(IssuedAtLayout, q.Get("issued_at"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "issued_at must be YYYY-MM-DDTHH:MM")
		return
	}
	found, err := h.svc.Lookup(r.Context(), p.SpaceID, Receipt{FN: q.Get("fn"), FD: q.Get("fd"), Kind: Kind(q.Get("kind")), Total: total, IssuedAt: at})
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := apitypes.ReceiptLookup{
		Receipt: nullable.NewNullNullable[apitypes.Receipt](), WrittenTo: nullable.NewNullNullable[apitypes.ReceiptMatch](),
		Candidates: make([]apitypes.ReceiptMatch, 0, len(found.Candidates)),
	}
	if found.Written != nil {
		out.Receipt = nullable.NewNullableWithValue(API(*found.Written))
	}
	if found.WrittenTo != nil {
		out.WrittenTo = nullable.NewNullableWithValue(match(*found.WrittenTo))
	}
	for _, op := range found.Candidates {
		out.Candidates = append(out.Candidates, match(op))
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.ReceiptNew
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	at, err := time.Parse(IssuedAtLayout, req.IssuedAt)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "issued_at must be YYYY-MM-DDTHH:MM")
		return
	}
	in := Receipt{FN: req.Fn, FD: req.Fd, Kind: Kind(req.Kind), IssuedAt: at, Total: req.TotalMinor, Source: string(req.Source)}
	if req.OperationId.IsSpecified() && !req.OperationId.IsNull() {
		id := req.OperationId.MustGet()
		in.OperationID = &id
	}
	if req.Fp.IsSpecified() && !req.Fp.IsNull() {
		fp := req.Fp.MustGet()
		in.FP = &fp
	}
	created, err := h.svc.Create(r.Context(), p.SpaceID, in)
	if errors.Is(err, ErrWritten) {
		httpjson.Error(w, http.StatusConflict, "the receipt is written already")
		return
	}
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, API(created))
}

func (h *Handler) handleAttach(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, err := uuid.Parse(r.PathValue("receiptId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "receiptId must be a uuid")
		return
	}
	var req apitypes.AttachReceiptRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	attached, err := h.svc.Attach(r.Context(), p.SpaceID, id, req.OperationId)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, API(attached))
}

func (h *Handler) handleResplit(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, err := uuid.Parse(r.PathValue("receiptId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "receiptId must be a uuid")
		return
	}
	split, err := h.svc.Resplit(r.Context(), p.SpaceID, id)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, apitypes.ReceiptSplitResult{Split: split})
}

// maxStatement is the largest statement taken: years of a family's receipts.
const maxStatement = 8 << 20

func (h *Handler) handleImport(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxStatement))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		httpjson.Error(w, http.StatusRequestEntityTooLarge, "the statement is larger than 8 MB")
		return
	}
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "the statement could not be read")
		return
	}
	receipts, err := ParseFNS(data)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	res, err := h.svc.Import(r.Context(), p.SpaceID, receipts)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, apitypes.ReceiptImportResult{
		Found: res.Found, Attached: res.Attached, Waiting: res.Waiting, Enriched: res.Enriched, Known: res.Known, Split: res.Split,
	})
}

// maxListed is the most rows or receipts one list asks for.
const maxListed = 200

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	q := r.URL.Query()
	var ids []uuid.UUID
	if raw := q.Get("operation_ids"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			id, err := uuid.Parse(strings.TrimSpace(part))
			if err != nil {
				httpjson.Error(w, http.StatusBadRequest, "operation_ids are row ids, comma-separated")
				return
			}
			ids = append(ids, id)
		}
	}
	if len(ids) > maxListed {
		httpjson.Error(w, http.StatusBadRequest, "at most 200 operation_ids")
		return
	}
	list, err := h.svc.List(r.Context(), p.SpaceID, ids, q.Get("waiting") == "true")
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := make([]apitypes.Receipt, 0, len(list))
	for _, rc := range list {
		out = append(out, API(rc))
	}
	httpjson.Write(w, http.StatusOK, out)
}

// API is a receipt as the API and the export write it.
func API(r Receipt) apitypes.Receipt {
	out := apitypes.Receipt{
		Id: r.ID, OperationId: nullable.NewNullNullable[uuid.UUID](), Fn: r.FN, Fd: r.FD, Fp: nullable.NewNullNullable[string](),
		Kind: apitypes.ReceiptKind(r.Kind), IssuedAt: r.IssuedAt.Format(IssuedAtLayout), TotalMinor: r.Total,
		Seller: nullable.NewNullNullable[string](), SellerInn: nullable.NewNullNullable[string](), Address: nullable.NewNullNullable[string](),
		Items: make([]apitypes.ReceiptItem, 0, len(r.Items)), Source: apitypes.ReceiptSource(r.Source),
	}
	if r.OperationID != nil {
		out.OperationId = nullable.NewNullableWithValue(*r.OperationID)
	}
	for _, f := range []struct {
		v   *string
		out *nullable.Nullable[string]
	}{{r.FP, &out.Fp}, {r.Seller, &out.Seller}, {r.SellerINN, &out.SellerInn}, {r.Address, &out.Address}} {
		if f.v != nil {
			*f.out = nullable.NewNullableWithValue(*f.v)
		}
	}
	for _, it := range r.Items {
		out.Items = append(out.Items, apitypes.ReceiptItem{Name: it.Name, Quantity: it.Quantity, PriceMinor: it.Price, SumMinor: it.Sum})
	}
	return out
}

func match(op operation.Operation) apitypes.ReceiptMatch {
	return apitypes.ReceiptMatch{
		Id: op.ID, AccountId: op.AccountID, OccurredOn: op.OccurredOn.Format(time.DateOnly),
		AmountMinor: op.AmountMinor, Currency: op.Currency,
	}
}

// toSignIn turns a refusal for want of a session into the sign-in page.
func toSignIn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&signInRedirect{ResponseWriter: w, r: r}, r)
	})
}

type signInRedirect struct {
	http.ResponseWriter
	r        *http.Request
	redirect bool
}

func (w *signInRedirect) WriteHeader(code int) {
	if code == http.StatusUnauthorized {
		w.redirect = true
		w.Header().Del("Content-Type")
		http.Redirect(w.ResponseWriter, w.r, "/login", http.StatusSeeOther)
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *signInRedirect) Write(b []byte) (int, error) {
	if w.redirect {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}
