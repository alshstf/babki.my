package mailbox

import (
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/receipt"
)

// Handler serves the mailbox read for receipts.
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
	srv.Mount("GET /api/v1/receipts/mailbox", view(h.handleGet))
	srv.Mount("PUT /api/v1/receipts/mailbox", edit(h.handlePut))
	srv.Mount("DELETE /api/v1/receipts/mailbox", edit(h.handleDelete))
	srv.Mount("POST /api/v1/receipts/mailbox/check", edit(h.handleCheck))
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	b, err := h.svc.Get(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, boxAPI(b))
}

func (h *Handler) handlePut(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.MailboxSettings
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	in := Settings{Host: req.Host, Port: req.Port, Username: req.Username, Folder: req.Folder}
	if req.Password.IsSpecified() && !req.Password.IsNull() {
		pw := req.Password.MustGet()
		in.Password = &pw
	}
	b, err := h.svc.Set(r.Context(), p.SpaceID, in)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, boxAPI(b))
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	if err := h.svc.Delete(r.Context(), p.SpaceID); err != nil {
		family.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleCheck(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	b, res, err := h.svc.Check(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, apitypes.MailboxCheck{Mailbox: boxAPI(b), Result: resultAPI(res)})
}

func boxAPI(b Box) apitypes.Mailbox {
	out := apitypes.Mailbox{
		Host: b.Host, Port: b.Port, Username: b.Username, Folder: b.Folder,
		CheckedAt: nullable.NewNullNullable[time.Time](), Problem: apitypes.MailboxProblem(b.Problem), LastFound: b.LastFound,
	}
	if b.CheckedAt != nil {
		out.CheckedAt = nullable.NewNullableWithValue(*b.CheckedAt)
	}
	return out
}

func resultAPI(r receipt.ImportResult) apitypes.ReceiptImportResult {
	return apitypes.ReceiptImportResult{Found: r.Found, Attached: r.Attached, Waiting: r.Waiting, Enriched: r.Enriched, Known: r.Known, Split: r.Split}
}
