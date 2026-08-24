package corporateaction

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler exposes the registry over HTTP.
//
// THE ROLES ARE THE CATALOG'S, and deliberately the same ones: a corporate
// action is a fact about a paper, exactly as a catalog row is, and both are
// instance-wide rather than a household's. So reading takes a viewer and
// writing takes an editor — which is to say an editor OR an owner, since
// family.RequireRole is a floor and Role.AtLeast lets the higher role through.
type Handler struct {
	store        *Store
	materializer *Materializer
	auth         *family.Auth
	sm           *scs.SessionManager
	log          *slog.Logger
}

func NewHandler(store *Store, materializer *Materializer, auth *family.Auth,
	sm *scs.SessionManager, log *slog.Logger,
) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{store: store, materializer: materializer, auth: auth, sm: sm, log: log}
}

func (h *Handler) Mount(srv *httpserver.Server) {
	view := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleViewer, fn)))
	}
	edit := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(family.RequireRole(family.RoleEditor, fn)))
	}
	srv.Mount("GET /api/v1/instrument-events", view(h.handleList))
	srv.Mount("POST /api/v1/instrument-events", edit(h.handleCreate))
	srv.Mount("DELETE /api/v1/instrument-events/{eventId}", edit(h.handleDelete))
}

// toAPI renders one event. resultCataloged says whether the catalog holds the
// paper this event produces — the caller answers it for a whole list in one
// query (see Store.CatalogedISINs), because it is what decides the one field
// here that is about this event rather than about its kind.
func toAPI(e Event, resultCataloged bool) apitypes.InstrumentEvent {
	out := apitypes.InstrumentEvent{
		Id:           e.ID,
		Kind:         apitypes.InstrumentEventKind(e.Kind),
		Isin:         e.ISIN,
		EffectiveOn:  openapi_types.Date{Time: e.EffectiveOn},
		RatioFrom:    e.RatioFrom,
		RatioTo:      e.RatioTo,
		Source:       apitypes.InstrumentEventSource(e.Source),
		SourceRef:    e.SourceRef,
		Note:         e.Note,
		Materialized: e.Kind.Materialized(),
		CreatedAt:    e.CreatedAt,
	}
	if reason := e.NotCountedReason(resultCataloged); reason != "" {
		published := apitypes.InstrumentEventNotCountedReason(reason)
		out.NotCountedReason = &published
	}
	if e.ResultISIN != "" {
		out.ResultIsin = nullable.NewNullableWithValue(e.ResultISIN)
	}
	if e.BasisShare != nil {
		out.BasisShare = nullable.NewNullableWithValue(e.BasisShare.String())
	}
	if e.MOEXSecID != "" {
		out.MoexSecid = &e.MOEXSecID
	}
	return out
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	events, err := h.store.List(r.Context())
	if err != nil {
		family.WriteError(w, err)
		return
	}
	results := make([]string, 0, len(events))
	for _, e := range events {
		results = append(results, e.ResultISIN)
	}
	cataloged, err := h.store.CatalogedISINs(r.Context(), results)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	out := make([]apitypes.InstrumentEvent, 0, len(events))
	for _, e := range events {
		out = append(out, toAPI(e, cataloged[e.ResultISIN]))
	}
	httpjson.Write(w, http.StatusOK, apitypes.InstrumentEventsResponse{Events: out})
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req apitypes.CreateInstrumentEventRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	p, _ := family.PrincipalFromContext(r.Context())
	e := Event{
		Kind:        Kind(req.Kind),
		ISIN:        req.Isin,
		EffectiveOn: req.EffectiveOn.Time,
		RatioFrom:   req.RatioFrom,
		RatioTo:     req.RatioTo,
		// ALWAYS manual, never read from the request. An exchange row is
		// written by the job that reads the exchange and rewritten by it on
		// every run; a request claiming to be one would produce a row nobody
		// could check, that the owner could not delete (Store.Delete refuses an
		// exchange row) and that the next job run would overwrite anyway.
		Source:    SourceManual,
		SourceRef: req.SourceRef,
		CreatedBy: &p.UserID,
	}
	if req.Note != nil {
		e.Note = *req.Note
	}
	if req.ResultIsin.IsSpecified() && !req.ResultIsin.IsNull() {
		e.ResultISIN = req.ResultIsin.MustGet()
	}
	if req.BasisShare.IsSpecified() && !req.BasisShare.IsNull() {
		share, err := decimal.NewFromString(req.BasisShare.MustGet())
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "basis_share must be a decimal string")
			return
		}
		e.BasisShare = &share
	}
	// Validated here as well as in the store's own callers, because this is the
	// door a person types at: Validate is the one statement of what an event has
	// to be, and running it before the insert turns every rule into a 400 that
	// names it instead of a constraint violation that does not.
	if err := e.Validate(); err != nil {
		family.WriteError(w, err)
		return
	}
	created, err := h.store.Create(r.Context(), e)
	if err != nil {
		family.WriteError(w, err)
		return
	}
	h.writeWithMaterialization(w, r, created, http.StatusCreated)
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("eventId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid eventId")
		return
	}
	removed, err := h.store.Delete(r.Context(), id)
	if errors.Is(err, errNoSuchEvent) {
		httpjson.Error(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		// ErrNotEditable (an exchange row) is a 400 naming the rule, and the
		// pgx.ErrNoRows that Store.Delete's own read returns for an id nobody
		// holds is a 404 — both by family.WriteError's table.
		family.WriteError(w, err)
		return
	}
	h.writeWithMaterialization(w, r, removed, http.StatusOK)
}

// writeWithMaterialization carries the registry into the journals and answers
// with what changed.
//
// IT RUNS INSIDE THE REQUEST rather than leaving it to the daily sweep, and the
// reason is what the owner does next: they record Amazon's 20:1 and go straight
// to the account to see whether the position is right. A sweep an hour later
// would mean the screen they open contradicts the row they just wrote, and
// nothing on it would say why.
//
// A FAILURE TO MATERIALIZE IS NOT A FAILURE TO RECORD. The event is stored by
// the time this runs, and it is the truth about the paper whether or not any
// journal could take it — the daily sweep will try again, and a refusal a
// journal makes is logged with the account it was made for. Answering 500 here
// would tell the owner their fact was not recorded, which would be false; the
// figures simply say nothing changed, and the log says why.
func (h *Handler) writeWithMaterialization(w http.ResponseWriter, r *http.Request,
	e Event, status int,
) {
	// A background context, deliberately: the write below must not be abandoned
	// half-done because the client hung up between the store's commit and this.
	// Bounded, because a request handler must not hold a connection for ever.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), materializeTimeout)
	defer cancel()

	stats, err := h.materializer.ForISIN(ctx, e.ISIN)
	if err != nil {
		h.log.Error("corporateaction: the event was recorded but its journal rows were not written",
			"event", e.ID, "isin", e.ISIN, "err", err)
	}
	queued := h.materializer.RequestRecheck(ctx, stats)
	// Asked AFTER the materialization rather than before: cataloguing the paper
	// and recording the event are two requests in either order, and the answer a
	// person reads on the row they just wrote must describe the world as it is
	// now. A failure to look it up is not worth failing the response over — the
	// event is written either way — so the row simply carries no reason.
	cataloged, err := h.store.CatalogedISINs(ctx, []string{e.ResultISIN})
	if err != nil {
		h.log.Error("corporateaction: could not tell whether the paper this event produces is in the catalog",
			"event", e.ID, "result_isin", e.ResultISIN, "err", err)
	}
	httpjson.Write(w, status, apitypes.InstrumentEventWritten{
		Event:           toAPI(e, cataloged[e.ResultISIN]),
		RowsAdded:       stats.Added,
		RowsRemoved:     stats.Removed,
		AccountsTouched: len(stats.Accounts),
		RecheckQueued:   queued,
	})
}

// materializeTimeout bounds the work one request does after its own write. A
// materialization folds one journal per holding account of one paper, which on
// the live instance is a handful; the bound is here so that a pathological
// instance answers slowly rather than never.
const materializeTimeout = 30 * time.Second
