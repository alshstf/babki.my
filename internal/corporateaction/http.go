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
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// Handler exposes the registry over HTTP. Roles are the catalog's: an event is
// an instance-wide fact about a paper, so reading needs a viewer and writing an
// editor or above.
type Handler struct {
	store        *Store
	materializer *Materializer
	// queue takes the retry of a materialization that failed in the request;
	// nil leaves it to the daily sweep.
	queue jobInserter
	auth  *family.Auth
	sm    *scs.SessionManager
	log   *slog.Logger
}

func NewHandler(store *Store, materializer *Materializer, queue jobInserter, auth *family.Auth,
	sm *scs.SessionManager, log *slog.Logger,
) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{store: store, materializer: materializer, queue: queue, auth: auth, sm: sm, log: log}
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

// toAPI renders one event; resultCataloged comes from one query for the whole
// list (Store.CatalogedISINs).
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
		// Always manual: exchange rows are written by the job only.
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
	// Validate here, at the door a person types at, so every rule is a 400
	// that names it. ISINs are upper-cased first; a malformed one is left for
	// Validate to refuse.
	if isin, err := instrument.NormalizeISIN(e.ISIN); err == nil {
		e.ISIN = isin
	}
	if isin, err := instrument.NormalizeISIN(e.ResultISIN); err == nil {
		e.ResultISIN = isin
	}
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
		// ErrNotEditable is a 400, pgx.ErrNoRows a 404 (family.WriteError).
		family.WriteError(w, err)
		return
	}
	h.writeWithMaterialization(w, r, removed, http.StatusOK)
}

// writeWithMaterialization carries the registry into the journals inside the
// request, so the account the owner opens next already shows the split, and
// answers with what changed. A failure to materialize is not a failure to record:
// the event is stored, a retry is queued, the sweep stands behind it, and the
// refusal is logged; a 500 would falsely say the fact was not recorded.
func (h *Handler) writeWithMaterialization(w http.ResponseWriter, r *http.Request,
	e Event, status int,
) {
	// Detached from the request so a client hanging up cannot abandon the
	// write midway; bounded so the handler cannot hold a connection forever.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), materializeTimeout)
	defer cancel()

	stats, err := h.materializer.ForISIN(ctx, e.ISIN)
	if err != nil {
		h.log.Error("corporateaction: the event was recorded but its journal rows were not written",
			"event", e.ID, "isin", e.ISIN, "err", err)
		h.retryLater(ctx, e.ISIN)
	}
	queued := h.materializer.RequestRecheck(ctx, stats)
	// Asked after materializing so the row describes the catalog as it is
	// now; a lookup failure just leaves the reason empty.
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

// retryLater queues another attempt; without a queue the sweep is the
// retry.
func (h *Handler) retryLater(ctx context.Context, isin string) {
	if h.queue == nil {
		return
	}
	if _, err := h.queue.Insert(ctx, MaterializeISINArgs{ISIN: isin}, MaterializeISINInsertOpts()); err != nil {
		h.log.Error("corporateaction: no retry could be queued, the journals wait for the daily sweep",
			"isin", isin, "err", err)
	}
}

// materializeTimeout bounds the work a request does after its write: one
// journal per holding account of one paper.
const materializeTimeout = 30 * time.Second
