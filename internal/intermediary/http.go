package intermediary

import (
	"net/http"

	"github.com/alexedwards/scs/v2"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler serves the catalog: GET /api/v1/intermediaries.
type Handler struct {
	auth *family.Auth
	sm   *scs.SessionManager
}

func NewHandler(auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	srv.Mount("GET /api/v1/intermediaries", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handleList)))))
}

func (h *Handler) handleList(w http.ResponseWriter, _ *http.Request) {
	c, err := Load()
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := apitypes.Intermediaries{CheckedOn: c.CheckedOn, Intermediaries: make([]apitypes.Intermediary, 0, len(c.All))}
	for _, i := range c.All {
		item := apitypes.Intermediary{
			Id: i.ID, Name: i.Name, Kind: apitypes.IntermediaryKind(i.Kind), Country: i.Country, Insured: i.Insured(),
			RegNumber: nullable.NewNullNullable[int](), BankId: nullable.NewNullNullable[string](), Aliases: i.Aliases,
		}
		if i.Reg > 0 {
			item.RegNumber = nullable.NewNullableWithValue(i.Reg)
		}
		if i.BankID != "" {
			item.BankId = nullable.NewNullableWithValue(i.BankID)
		}
		if item.Aliases == nil {
			item.Aliases = []string{}
		}
		out.Intermediaries = append(out.Intermediaries, item)
	}
	httpjson.Write(w, http.StatusOK, out)
}
