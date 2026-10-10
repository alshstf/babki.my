package structure

import (
	"net/http"

	"github.com/alexedwards/scs/v2"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler serves GET /api/v1/structure.
type Handler struct {
	svc  *Service
	auth *family.Auth
	sm   *scs.SessionManager
}

func NewHandler(svc *Service, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{svc: svc, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	srv.Mount("GET /api/v1/structure", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handleStructure)))))
}

func (h *Handler) handleStructure(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	valuation := Liquid
	if raw := r.URL.Query().Get("valuation"); raw != "" {
		valuation = Valuation(raw)
	}
	s, err := h.svc.Of(r.Context(), p.SpaceID, valuation)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	slicesAPI := func(in []Slice) []apitypes.StructureSlice {
		out := make([]apitypes.StructureSlice, 0, len(in))
		for _, x := range in {
			out = append(out, apitypes.StructureSlice{Key: x.Key, Minor: x.Minor})
		}
		return out
	}
	httpjson.Write(w, http.StatusOK, apitypes.Structure{
		BaseCurrency: s.BaseCurrency, Valuation: apitypes.StructureValuation(s.Valuation),
		AssetsMinor: s.Assets, DebtsMinor: s.Debts,
		ByClass: slicesAPI(s.ByClass), ByCurrency: slicesAPI(s.ByCurrency),
		ByAccount: slicesAPI(s.ByAccount), ByCountry: slicesAPI(s.ByCountry),
		Unpriced: s.Unpriced, MissingRates: s.MissingRates,
	})
}
