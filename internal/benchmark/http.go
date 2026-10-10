package benchmark

import (
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/dates"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler serves GET /api/v1/return/benchmarks.
type Handler struct {
	svc  *Service
	auth *family.Auth
	sm   *scs.SessionManager
}

func NewHandler(svc *Service, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{svc: svc, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	srv.Mount("GET /api/v1/return/benchmarks", h.sm.LoadAndSave(h.auth.RequireAuth(
		family.RequireRole(family.RoleViewer, http.HandlerFunc(h.handleCompare)))))
}

func (h *Handler) handleCompare(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	from, errFrom := time.Parse(time.DateOnly, r.URL.Query().Get("from"))
	to, errTo := time.Parse(time.DateOnly, r.URL.Query().Get("to"))
	if errFrom != nil || errTo != nil || !from.Before(to) || to.After(dates.LatestRecordable()) {
		httpjson.Error(w, http.StatusBadRequest, "from and to must be YYYY-MM-DD, from before to, to at most today")
		return
	}
	base, results, err := h.svc.Compare(r.Context(), p.SpaceID, from, to)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := apitypes.Benchmarks{
		Currency: base, From: from.Format(time.DateOnly), To: to.Format(time.DateOnly),
		Benchmarks: make([]apitypes.Benchmark, 0, len(results)),
	}
	for _, res := range results {
		b := apitypes.Benchmark{
			Code: apitypes.BenchmarkCode(res.Code), IndexCurrency: res.Currency, Complete: res.Complete,
			EndMinor: nullable.NewNullNullable[int64](), AnnualRate: nullable.NewNullNullable[string](),
		}
		if res.Complete {
			b.EndMinor = nullable.NewNullableWithValue(res.End)
		}
		if res.AnnualRate != nil {
			b.AnnualRate = nullable.NewNullableWithValue(decimal.NewFromFloat(*res.AnnualRate).Round(4).String())
		}
		out.Benchmarks = append(out.Benchmarks, b)
	}
	httpjson.Write(w, http.StatusOK, out)
}
