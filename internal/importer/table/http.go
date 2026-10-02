package table

import (
	"net/http"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler exposes table imports over HTTP.
type Handler struct {
	svc  *Service
	auth *family.Auth
	sm   *scs.SessionManager
}

func NewHandler(svc *Service, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{svc: svc, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	edit := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleEditor, fn)))
	}
	srv.Mount("POST /api/v1/accounts/{accountId}/imports/preview", edit(h.handlePreview))
}

func (h *Handler) handlePreview(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	accountID, err := uuid.Parse(r.PathValue("accountId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid accountId")
		return
	}
	var req apitypes.ImportPreviewRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	var mapping *Mapping
	if req.Mapping != nil {
		m := mappingFromAPI(*req.Mapping)
		mapping = &m
	}
	preview, err := h.svc.Preview(r.Context(), p.SpaceID, accountID, req.Content, mapping)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, previewToAPI(preview))
}

func mappingFromAPI(in apitypes.ImportMapping) Mapping {
	m := Mapping{HasHeader: in.HasHeader, Columns: map[Field]int{}, Types: map[string]operation.Type{}}
	for field, col := range in.Columns {
		m.Columns[Field(field)] = col
	}
	for cell, typ := range in.Types {
		m.Types[typeKey(cell)] = operation.Type(typ)
	}
	return m
}

func mappingToAPI(m Mapping) apitypes.ImportMapping {
	out := apitypes.ImportMapping{
		HasHeader: m.HasHeader,
		Columns:   map[string]int{},
		Types:     map[string]apitypes.OperationType{},
	}
	for field, col := range m.Columns {
		out.Columns[string(field)] = col
	}
	for cell, typ := range m.Types {
		out.Types[cell] = apitypes.OperationType(typ)
	}
	return out
}

func previewToAPI(p Preview) apitypes.ImportPreview {
	out := apitypes.ImportPreview{
		Mapping: mappingToAPI(p.Mapping),
		Header:  p.Header,
		Rows:    make([]apitypes.ImportRow, 0, len(p.Rows)),
	}
	if out.Header == nil {
		out.Header = []string{}
	}
	for _, row := range p.Rows {
		item := apitypes.ImportRow{
			Line:      row.Line.Number,
			Cells:     row.Line.Cells,
			Verdict:   apitypes.ImportRowVerdict(row.Verdict),
			Reason:    nullable.NewNullNullable[string](),
			Operation: nullable.NewNullNullable[apitypes.ImportedOperation](),
		}
		if row.Reason != "" {
			item.Reason = nullable.NewNullableWithValue(row.Reason)
		}
		if op := row.Operation; op != nil {
			o := apitypes.ImportedOperation{
				Type:         apitypes.OperationType(op.Type),
				OccurredOn:   op.OccurredOn.Format("2006-01-02"),
				AmountMinor:  op.AmountMinor,
				Currency:     op.Currency,
				FeeMinor:     op.FeeMinor,
				Note:         op.Note,
				InstrumentId: nullable.NewNullNullable[uuid.UUID](),
				Quantity:     nullable.NewNullNullable[string](),
				Price:        nullable.NewNullNullable[string](),
			}
			if op.InstrumentID != nil {
				o.InstrumentId = nullable.NewNullableWithValue(*op.InstrumentID)
			}
			if op.Quantity != nil {
				o.Quantity = nullable.NewNullableWithValue(op.Quantity.String())
			}
			if op.Price != nil {
				o.Price = nullable.NewNullableWithValue(op.Price.String())
			}
			item.Operation = nullable.NewNullableWithValue(o)
		}
		out.Rows = append(out.Rows, item)
	}
	return out
}
