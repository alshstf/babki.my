package tinvest

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
)

// defaultPageLimit and maxPageLimit bound the two paged endpoints below, and
// they are the figures api/openapi.yaml states as `default` and `maximum` on
// both of them (internal/importer/tinvest/contract_sites_test.go is what keeps
// the two spellings in step).
//
// A LIMIT ABOVE THE MAXIMUM IS REFUSED, NOT CLAMPED. The journal listing does
// clamp, and #118 is the record of why that is a defect rather than a
// convenience: a ceiling the contract states and the server does not apply is
// not a rule, and a client that sent 250 would be answered as though it had
// asked for 200 with nothing in the answer saying so. Refusing says it once, in
// the only channel a client may read — the status code — and costs a round trip
// on a request that was outside the contract to begin with.
const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// Handler exposes the T-Invest importer over HTTP.
//
// IT HOLDS NO STORE. Every path here goes through Service, including the reads,
// because every one of them is owner-only and the role check lives there (see
// the Service doc, and family.Service.UpdateSpace for the same arrangement in
// the module this one copies). A handler with a store beside the service is a
// handler where the next read can quietly be written without a role check and
// nothing will notice.
type Handler struct {
	svc  *Service
	auth *family.Auth
	sm   *scs.SessionManager
}

func NewHandler(svc *Service, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{svc: svc, auth: auth, sm: sm}
}

// Mount registers the importer's routes.
//
// The middleware requires a signed-in member and NOTHING MORE: the owner-only
// rule is Service's, applied to every method it has, and stating it here as well
// would be a second copy of one rule — the kind that eventually disagrees with
// the first. What the middleware still owes is the session and the principal,
// without which the service could not check anything at all.
func (h *Handler) Mount(srv *httpserver.Server) {
	authed := func(fn http.HandlerFunc) http.Handler {
		return h.sm.LoadAndSave(h.auth.RequireAuth(fn))
	}
	srv.Mount("POST /api/v1/tinvest/token-check", authed(h.handleTokenCheck))
	srv.Mount("GET /api/v1/tinvest/connections", authed(h.handleList))
	srv.Mount("POST /api/v1/tinvest/connections", authed(h.handleCreate))
	srv.Mount("GET /api/v1/tinvest/connections/{connectionId}", authed(h.handleGet))
	srv.Mount("PATCH /api/v1/tinvest/connections/{connectionId}", authed(h.handleUpdate))
	srv.Mount("DELETE /api/v1/tinvest/connections/{connectionId}", authed(h.handleDelete))
	srv.Mount("POST /api/v1/tinvest/connections/{connectionId}/sync", authed(h.handleSync))
	srv.Mount("GET /api/v1/tinvest/connections/{connectionId}/runs", authed(h.handleRuns))
	srv.Mount("GET /api/v1/tinvest/connections/{connectionId}/unparsed", authed(h.handleUnparsed))
}

// writeError maps this package's own sentinels onto status codes and hands
// everything else to family.WriteError.
//
// THE THREE PAIRS BELOW ARE THE WHOLE POINT OF HAVING SENTINELS AT ALL. A
// refused token (400) and an unreachable broker (502) are opposite advice —
// paste a new token, or wait — and a client can only tell them apart by the
// code, since the text is prose. A connection that is switched off (409) is not
// a missing one (404). And a broker account already imported (409) is not a
// malformed request (400).
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrTokenRejected), errors.Is(err, ErrBrokerAccountNotImportable):
		httpjson.Error(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrConnectionNotActive), errors.Is(err, ErrBrokerAccountAlreadyLinked):
		httpjson.Error(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrBrokerUnreachable):
		// Logged as well as answered, through the same default logger
		// family.WriteError uses for the errors behind its 500s: this branch
		// never reaches that function, so without the line the one failure an
		// operator would most want in the log — the broker being down — leaves
		// no trace of what actually went wrong beyond a 502 the user saw.
		slog.Default().Error("tinvest: request failed at the broker", "err", err.Error())
		httpjson.Error(w, http.StatusBadGateway, err.Error())
	default:
		family.WriteError(w, err)
	}
}

func pathConnectionID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("connectionId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid connectionId")
		return uuid.Nil, false
	}
	return id, true
}

// parsePage reads `limit` and `offset` and enforces the bounds the contract
// states. See defaultPageLimit for why an over-large limit is a 400 rather than
// a quietly smaller page.
func parsePage(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	limit = defaultPageLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxPageLimit {
			httpjson.Error(w, http.StatusBadRequest,
				fmt.Sprintf("limit must be a whole number from 1 to %d", maxPageLimit))
			return 0, 0, false
		}
		limit = n
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			httpjson.Error(w, http.StatusBadRequest, "offset must be a whole number of at least 0")
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

// dateOrNull renders an optional calendar date the way every other date in this
// API is rendered: YYYY-MM-DD, or an explicit null. An explicit null and not an
// omitted key: "the broker told us nothing about when this account was opened"
// is a statement, and a missing field would read as this server having forgotten
// to answer.
func dateOrNull(t *time.Time) nullable.Nullable[string] {
	if t == nil {
		return nullable.NewNullNullable[string]()
	}
	return nullable.NewNullableWithValue(t.Format("2006-01-02"))
}

// timeOrNull is dateOrNull for the instants: an absent one is an explicit null,
// for the same reason.
func timeOrNull(t *time.Time) nullable.Nullable[time.Time] {
	if t == nil {
		return nullable.NewNullNullable[time.Time]()
	}
	return nullable.NewNullableWithValue(*t)
}

func brokerAccountAPI(a Account) apitypes.TinvestBrokerAccount {
	return apitypes.TinvestBrokerAccount{
		BrokerAccountId: a.ID,
		Name:            a.Name,
		Type:            a.Type,
		OpenedOn:        dateOrNull(a.OpenedOn),
	}
}

func linkAPI(l AccountLink) apitypes.TinvestLinkedAccount {
	return apitypes.TinvestLinkedAccount{
		LinkId:            l.ID,
		AccountId:         l.AccountID,
		BrokerAccountId:   l.BrokerAccountID,
		BrokerAccountName: l.BrokerAccountName,
		BrokerAccountType: l.BrokerAccountType,
		OpenedOn:          dateOrNull(l.OpenedOn),
	}
}

// mismatchesAPI decodes the differences a run recorded. The column holds what
// json.Marshal wrote from []ReconcileMismatch, so this round-trips; a value that
// will not decode is corruption and is reported rather than published as an
// empty list, which would read as "the check found nothing".
//
// A null column — a run nobody checked — decodes into no elements and no error,
// and comes back as the empty list the contract requires. What tells that apart
// from a check that found nothing is reconcile_status, published beside it.
func mismatchesAPI(raw json.RawMessage) ([]apitypes.TinvestReconcileMismatch, error) {
	var list []ReconcileMismatch
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("tinvest: decode the differences a run recorded: %w", err)
		}
	}
	out := make([]apitypes.TinvestReconcileMismatch, 0, len(list))
	for _, m := range list {
		item := apitypes.TinvestReconcileMismatch{
			Kind:    apitypes.TinvestReconcileMismatchKind(m.Kind),
			Label:   m.Label,
			Broker:  m.Broker.String(),
			Journal: m.Journal.String(),
		}
		if m.InstrumentID != nil {
			item.InstrumentId = nullable.NewNullableWithValue(*m.InstrumentID)
		} else {
			item.InstrumentId = nullable.NewNullNullable[uuid.UUID]()
		}
		out = append(out, item)
	}
	return out, nil
}

func runAPI(r SyncRun) (apitypes.TinvestSyncRun, error) {
	mismatches, err := mismatchesAPI(r.ReconcileMismatches)
	if err != nil {
		return apitypes.TinvestSyncRun{}, err
	}
	out := apitypes.TinvestSyncRun{
		Id:               r.ID,
		LinkId:           r.LinkID,
		Trigger:          apitypes.TinvestSyncTrigger(r.Trigger),
		Status:           apitypes.TinvestSyncRunStatus(r.Status),
		StartedAt:        r.StartedAt,
		ReadCount:        r.ReadCount,
		AddedCount:       r.AddedCount,
		DisappearedCount: r.DisappearedCount,
		UnparsedCount:    r.UnparsedCount,
		Error:            r.Error,
		ReconcileStatus:  apitypes.TinvestReconcileStatus(r.ReconcileStatus),
		Mismatches:       mismatches,
	}
	out.FinishedAt = timeOrNull(r.FinishedAt)
	out.ReconciledAt = timeOrNull(r.ReconciledAt)
	return out, nil
}

func connectionAPI(v ConnectionView) (apitypes.TinvestConnection, error) {
	out := apitypes.TinvestConnection{
		Id:                   v.Connection.ID,
		Status:               apitypes.TinvestConnectionStatus(v.Connection.Status),
		TokenLast4:           v.Connection.TokenLast4,
		Accounts:             make([]apitypes.TinvestLinkedAccount, 0, len(v.Links)),
		LastSuccessfulSyncAt: timeOrNull(v.LastSuccessfulSyncAt),
	}
	for _, l := range v.Links {
		out.Accounts = append(out.Accounts, linkAPI(l))
	}
	out.LastReconcile = nullable.NewNullNullable[apitypes.TinvestReconcileSnapshot]()
	if v.LastReconcile != nil && v.LastReconcile.ReconciledAt != nil {
		mismatches, err := mismatchesAPI(v.LastReconcile.ReconcileMismatches)
		if err != nil {
			return apitypes.TinvestConnection{}, err
		}
		out.LastReconcile = nullable.NewNullableWithValue(apitypes.TinvestReconcileSnapshot{
			At:         *v.LastReconcile.ReconciledAt,
			Status:     apitypes.TinvestReconcileStatus(v.LastReconcile.ReconcileStatus),
			Mismatches: mismatches,
		})
	}
	return out, nil
}

func (h *Handler) handleTokenCheck(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.TinvestTokenCheckRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	accounts, err := h.svc.CheckToken(r.Context(), p, req.Token)
	if err != nil {
		writeError(w, err)
		return
	}
	out := apitypes.TinvestTokenCheckResponse{
		Accounts: make([]apitypes.TinvestBrokerAccount, 0, len(accounts)),
	}
	for _, a := range accounts {
		out.Accounts = append(out.Accounts, brokerAccountAPI(a))
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	views, err := h.svc.ListConnections(r.Context(), p)
	if err != nil {
		writeError(w, err)
		return
	}
	out := apitypes.TinvestConnectionsResponse{
		Connections: make([]apitypes.TinvestConnection, 0, len(views)),
	}
	for _, v := range views {
		api, err := connectionAPI(v)
		if err != nil {
			writeError(w, err)
			return
		}
		out.Connections = append(out.Connections, api)
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	var req apitypes.CreateTinvestConnectionRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	picks := make([]AccountPick, 0, len(req.Accounts))
	for _, a := range req.Accounts {
		picks = append(picks, AccountPick{BrokerAccountID: a.BrokerAccountId, AccountName: a.AccountName})
	}
	view, err := h.svc.CreateConnection(r.Context(), p, req.Token, picks)
	if err != nil {
		writeError(w, err)
		return
	}
	h.writeConnection(w, http.StatusCreated, view)
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathConnectionID(w, r)
	if !ok {
		return
	}
	view, err := h.svc.Connection(r.Context(), p, id)
	if err != nil {
		writeError(w, err)
		return
	}
	h.writeConnection(w, http.StatusOK, view)
}

func (h *Handler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathConnectionID(w, r)
	if !ok {
		return
	}
	var req apitypes.UpdateTinvestConnectionRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	upd := ConnectionUpdate{Token: req.Token}
	if req.Status != nil {
		st := ConnectionStatus(*req.Status)
		upd.Status = &st
	}
	view, err := h.svc.UpdateConnection(r.Context(), p, id, upd)
	if err != nil {
		writeError(w, err)
		return
	}
	h.writeConnection(w, http.StatusOK, view)
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathConnectionID(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteConnection(r.Context(), p, id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleSync(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathConnectionID(w, r)
	if !ok {
		return
	}
	queued, err := h.svc.TriggerSync(r.Context(), p, id)
	if err != nil {
		writeError(w, err)
		return
	}
	// 202 in both cases: the sync is queued, never performed here. `queued`
	// says whether THIS request is what put it there — see the field's own
	// description for why a false is not "one is running".
	httpjson.Write(w, http.StatusAccepted, apitypes.TinvestSyncAcceptedResponse{Queued: queued})
}

func (h *Handler) handleRuns(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathConnectionID(w, r)
	if !ok {
		return
	}
	limit, offset, ok := parsePage(w, r)
	if !ok {
		return
	}
	runs, hasMore, err := h.svc.Runs(r.Context(), p, id, limit, offset)
	if err != nil {
		writeError(w, err)
		return
	}
	out := apitypes.TinvestSyncRunsResponse{
		Runs:    make([]apitypes.TinvestSyncRun, 0, len(runs)),
		HasMore: hasMore,
	}
	for _, run := range runs {
		api, err := runAPI(run)
		if err != nil {
			writeError(w, err)
			return
		}
		out.Runs = append(out.Runs, api)
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (h *Handler) handleUnparsed(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathConnectionID(w, r)
	if !ok {
		return
	}
	limit, offset, ok := parsePage(w, r)
	if !ok {
		return
	}
	rows, hasMore, err := h.svc.Unparsed(r.Context(), p, id, limit, offset)
	if err != nil {
		writeError(w, err)
		return
	}
	out := apitypes.TinvestUnparsedResponse{
		Operations: make([]apitypes.TinvestUnparsedOperation, 0, len(rows)),
		HasMore:    hasMore,
	}
	for _, m := range rows {
		out.Operations = append(out.Operations, apitypes.TinvestUnparsedOperation{
			Id:          m.ID,
			OccurredAt:  m.OccurredAt,
			OpType:      m.OpType,
			Payment:     m.Payment.String(),
			Currency:    m.Currency,
			Description: m.Description,
			Reason:      apitypes.TinvestUnparsedReason(m.UnparsedReason),
			// The broker's own bytes, handed on rather than re-encoded: nothing
			// here computes from them, and re-modelling them would be a second
			// reading of a document this program deliberately keeps unread.
			Raw: m.Raw,
		})
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (h *Handler) writeConnection(w http.ResponseWriter, status int, view ConnectionView) {
	api, err := connectionAPI(view)
	if err != nil {
		writeError(w, err)
		return
	}
	httpjson.Write(w, status, api)
}
