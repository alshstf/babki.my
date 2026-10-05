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
	openapi_types "github.com/oapi-codegen/runtime/types"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/tradingmode"
)

// defaultPageLimit and maxPageLimit are the default and maximum openapi states
// for the two paged endpoints (contract_sites_test.go keeps them in step). A limit
// outside them is refused, not clamped (#118).
const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// Handler exposes the T-Invest importer over HTTP. It holds no store: every
// path goes through Service, where the owner-only check lives, so no read can
// skip it.
type Handler struct {
	svc  *Service
	auth *family.Auth
	sm   *scs.SessionManager
}

func NewHandler(svc *Service, auth *family.Auth, sm *scs.SessionManager) *Handler {
	return &Handler{svc: svc, auth: auth, sm: sm}
}

// Mount registers the routes. The middleware requires a signed-in member
// only; the owner rule is Service's.
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
	srv.Mount("POST /api/v1/tinvest/links/{linkId}/explanations", authed(h.handleExplain))
	srv.Mount("DELETE /api/v1/tinvest/explanations/{explanationId}", authed(h.handleRemoveExplanation))
}

// writeError maps this package's sentinels to status codes and hands the rest
// to family.WriteError. The codes are the advice: a refused token (400) means
// paste a new one, an unreachable broker (502) means wait; a disabled connection
// (409) is not a missing one (404); an already imported account (409) is not a
// bad request. A picked account the token cannot import is 422, not 400: the
// broker's list may have changed since the wizard checked the token, which still
// works (Service.CreateConnection, the only path declaring it).
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrTokenRejected):
		httpjson.Error(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrBrokerAccountNotImportable):
		httpjson.Error(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, ErrConnectionNotActive), errors.Is(err, ErrBrokerAccountAlreadyLinked),
		errors.Is(err, ErrRowAlreadyExplained), errors.Is(err, operation.ErrInconsistent):
		// The journal's refusal answered as the journal answers it: 409 with the
		// engine's sentence, not a 500 (see operation.writeError).
		httpjson.Error(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrRowNotInLink), errors.Is(err, ErrExplanationNotFound), errors.Is(err, ErrLinkNotFound):
		// 404: each names something well formed this space does not have.
		httpjson.Error(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrBrokerUnreachable):
		// Logged too: this branch skips family.WriteError, and a broker outage
		// should leave a trace beyond the 502.
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

// parsePage reads limit and offset and refuses anything outside the stated
// bounds.
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

// dateOrNull renders an optional date as YYYY-MM-DD or an explicit null: "the
// broker told us nothing" is a statement, not a forgotten field.
func dateOrNull(t *time.Time) nullable.Nullable[string] {
	if t == nil {
		return nullable.NewNullNullable[string]()
	}
	return nullable.NewNullableWithValue(t.Format("2006-01-02"))
}

// nullableString is dateOrNull for an optional text.
func nullableString(s *string) nullable.Nullable[string] {
	if s == nil {
		return nullable.NewNullNullable[string]()
	}
	return nullable.NewNullableWithValue(*s)
}

// timeOrNull is dateOrNull for an instant.
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

// mismatchesAPI decodes a run's recorded differences. A value that will not
// decode is corruption and reported, not published as "nothing found". A null
// column (unchecked run) is an empty list; reconcile_status tells the two
// apart.
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
		// The passport fields are null when the check recorded none, including
		// runs from before they existed.
		item.BrokerIsin = nullableString(m.BrokerISIN)
		item.BrokerName = nullableString(m.BrokerName)
		item.BrokerCurrency = nullableString(m.BrokerCurrency)
		if m.BrokerType != nil {
			item.BrokerType = nullable.NewNullableWithValue(apitypes.InstrumentType(*m.BrokerType))
		} else {
			item.BrokerType = nullable.NewNullNullable[apitypes.InstrumentType]()
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
	// One verdict per linked account, built from the links themselves, so an
	// unchecked account shows as not_checked rather than missing, under the
	// same name.
	out.Reconciles = make([]apitypes.TinvestAccountReconcile, 0, len(v.Links))
	for _, l := range v.Links {
		item := apitypes.TinvestAccountReconcile{
			LinkId:            l.ID,
			AccountId:         l.AccountID,
			BrokerAccountName: l.BrokerAccountName,
			Status:            apitypes.TinvestReconcileStatus(ReconcileNotChecked),
			At:                nullable.NewNullNullable[time.Time](),
			Mismatches:        []apitypes.TinvestReconcileMismatch{},
			// On every verdict: a fact about the account, needed to read a cash
			// difference.
			CurrencyTradesUnparsed: v.CurrencyTradesUnparsedByLink[l.ID],
		}
		if run, ok := v.LastReconcileByLink[l.ID]; ok {
			mismatches, err := mismatchesAPI(run.ReconcileMismatches)
			if err != nil {
				return apitypes.TinvestConnection{}, err
			}
			item.Status = apitypes.TinvestReconcileStatus(run.ReconcileStatus)
			item.At = timeOrNull(run.ReconciledAt)
			item.Mismatches = mismatches
		}
		out.Reconciles = append(out.Reconciles, item)
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
	// 202 either way; queued says whether this request queued it.
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
		row := apitypes.TinvestUnparsedOperation{
			Id: m.ID,
			// Which linked account the row is on.
			LinkId: m.LinkID,
			// What an explanation names the row by: the broker's operation id may
			// change (see MirrorRow.ContentKey).
			ContentKey: m.ContentKey,
			OccurredAt: m.OccurredAt,
			OpType:     m.OpType,
			// The code as sent (empty when none) and its kind, unknown for empty:
			// this list shows what the broker sent.
			ClassCode:       m.ClassCode,
			TradingModeKind: nullable.NewNullableWithValue(apitypes.TradingModeKind(tradingmode.Of(m.ClassCode))),
			Payment:         m.Payment.String(),
			Currency:        m.Currency,
			Description:     m.Description,
			Reason:          apitypes.TinvestUnparsedReason(m.UnparsedReason),
			// The refuser's own words: shown, never read.
			Detail: m.UnparsedDetail,
			// The broker's own bytes, not re-encoded.
			Raw: m.Raw,
			// Explicitly null; unset would be the zero time.
			DisappearedAt: nullable.NewNullNullable[time.Time](),
		}
		if m.DisappearedAt != nil {
			row.DisappearedAt = nullable.NewNullableWithValue(*m.DisappearedAt)
		}
		if e := m.ExplainedBy; e != nil {
			row.ExplainedBy.Set(apitypes.TinvestRowExplanation{
				Id:            e.ID,
				OperationId:   e.OperationID,
				OperationOn:   openapi_types.Date{Time: e.OperationOn},
				OperationType: apitypes.OperationType(e.OperationType),
			})
		}
		out.Operations = append(out.Operations, row)
	}
	httpjson.Write(w, http.StatusOK, out)
}

// handleExplain records that one manual operation accounts for the named
// broker rows of a linked account.
func (h *Handler) handleExplain(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	linkID, err := uuid.Parse(r.PathValue("linkId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid linkId")
		return
	}
	var req apitypes.TinvestExplainRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	// The journal's own request parser, not a second one.
	op, err := operation.OperationFromCreateRequest(req.Operation)
	if err != nil {
		var bad operation.BadFieldError
		if errors.As(err, &bad) {
			httpjson.Error(w, http.StatusBadRequest, bad.Message)
			return
		}
		writeError(w, err)
		return
	}

	explanation, queued, err := h.svc.ExplainRows(r.Context(), p, linkID, req.ContentKeys, op)
	if err != nil {
		writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, apitypes.TinvestExplanationResponse{
		OperationId: explanation.OperationID,
		SyncQueued:  queued,
	})
}

// handleRemoveExplanation deletes an explanation and its manual operation
// (see Service.RemoveExplanation).
func (h *Handler) handleRemoveExplanation(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, err := uuid.Parse(r.PathValue("explanationId"))
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid explanationId")
		return
	}
	queued, err := h.svc.RemoveExplanation(r.Context(), p, id)
	if err != nil {
		writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, apitypes.TinvestExplanationRemoved{SyncQueued: queued})
}

func (h *Handler) writeConnection(w http.ResponseWriter, status int, view ConnectionView) {
	api, err := connectionAPI(view)
	if err != nil {
		writeError(w, err)
		return
	}
	httpjson.Write(w, status, api)
}
