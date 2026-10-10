package category

import (
	"net/http"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler serves a family's categories.
type Handler struct {
	store *Store
	auth  *family.Auth
	sm    *scs.SessionManager
}

func NewHandler(store *Store, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{store: store, auth: auth, sm: sm}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	view := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleViewer, fn)))
	}
	edit := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleEditor, fn)))
	}
	srv.Mount("GET /api/v1/categories", view(h.handleList))
	srv.Mount("POST /api/v1/categories", edit(h.handleCreate))
	srv.Mount("PATCH /api/v1/categories/{categoryId}", edit(h.handleUpdate))
	srv.Mount("DELETE /api/v1/categories/{categoryId}", edit(h.handleDelete))
	srv.Mount("GET /api/v1/category-rules", view(h.handleListRules))
	srv.Mount("POST /api/v1/category-rules", edit(h.handleCreateRule))
	srv.Mount("PUT /api/v1/category-rules/order", edit(h.handleReorderRules))
	srv.Mount("PATCH /api/v1/category-rules/{ruleId}", edit(h.handleUpdateRule))
	srv.Mount("DELETE /api/v1/category-rules/{ruleId}", edit(h.handleDeleteRule))
}

func toAPI(c Category) apitypes.Category {
	out := apitypes.Category{
		Id: c.ID, Kind: apitypes.CategoryKind(c.Kind), Name: c.Name,
		Archived: c.Archived, Position: c.Position, ParentId: nullable.NewNullNullable[uuid.UUID](),
	}
	if c.ParentID != nil {
		out.ParentId = nullable.NewNullableWithValue(*c.ParentID)
	}
	return out
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	list, err := h.store.List(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := make([]apitypes.Category, 0, len(list))
	for _, c := range list {
		out = append(out, toAPI(c))
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req apitypes.CreateCategoryRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	var parent *uuid.UUID
	if req.ParentId.IsSpecified() && !req.ParentId.IsNull() {
		id := req.ParentId.MustGet()
		parent = &id
	}
	p, _ := family.PrincipalFromContext(r.Context())
	c, err := h.store.Create(r.Context(), p.SpaceID, Kind(req.Kind), req.Name, parent)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, toAPI(c))
}

func (h *Handler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req apitypes.UpdateCategoryRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	upd := Update{Name: req.Name, Archived: req.Archived, Position: req.Position}
	if req.ParentId.IsSpecified() {
		var parent *uuid.UUID
		if !req.ParentId.IsNull() {
			v := req.ParentId.MustGet()
			parent = &v
		}
		upd.ParentID = &parent
	}
	p, _ := family.PrincipalFromContext(r.Context())
	c, err := h.store.Update(r.Context(), p.SpaceID, id, upd)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, toAPI(c))
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, _ := family.PrincipalFromContext(r.Context())
	if err := h.store.Delete(r.Context(), p.SpaceID, id); err != nil {
		family.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("categoryId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "categoryId must be a UUID")
		return uuid.UUID{}, false
	}
	return id, true
}

func ruleAPI(r Rule) apitypes.CategoryRule {
	return apitypes.CategoryRule{
		Id: r.ID, CategoryId: r.CategoryID, Field: apitypes.CategoryRuleField(r.Field),
		Pattern: r.Pattern, Position: r.Position,
	}
}

func (h *Handler) handleListRules(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	rules, err := h.store.Rules(r.Context(), p.SpaceID)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := make([]apitypes.CategoryRule, 0, len(rules))
	for _, rule := range rules {
		out = append(out, ruleAPI(rule))
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (h *Handler) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var req apitypes.CreateCategoryRuleRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	p, _ := family.PrincipalFromContext(r.Context())
	rule, err := h.store.CreateRule(r.Context(), p.SpaceID, Rule{CategoryID: req.CategoryId, Field: Field(req.Field), Pattern: req.Pattern})
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, ruleAPI(rule))
}

func (h *Handler) handleReorderRules(w http.ResponseWriter, r *http.Request) {
	var req apitypes.ReorderCategoryRulesRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	p, _ := family.PrincipalFromContext(r.Context())
	if err := h.store.ReorderRules(r.Context(), p.SpaceID, req.Ids); err != nil {
		family.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathRuleID(w, r)
	if !ok {
		return
	}
	var req apitypes.UpdateCategoryRuleRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	upd := RuleUpdate{CategoryID: req.CategoryId, Pattern: req.Pattern}
	if req.Field != nil {
		f := Field(*req.Field)
		upd.Field = &f
	}
	p, _ := family.PrincipalFromContext(r.Context())
	rule, err := h.store.UpdateRule(r.Context(), p.SpaceID, id, upd)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, ruleAPI(rule))
}

func (h *Handler) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathRuleID(w, r)
	if !ok {
		return
	}
	p, _ := family.PrincipalFromContext(r.Context())
	if err := h.store.DeleteRule(r.Context(), p.SpaceID, id); err != nil {
		family.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func pathRuleID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("ruleId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "ruleId must be a UUID")
		return uuid.UUID{}, false
	}
	return id, true
}
