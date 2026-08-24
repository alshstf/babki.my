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
	srv.Mount("POST /api/v1/tinvest/links/{linkId}/explanations", authed(h.handleExplain))
	srv.Mount("DELETE /api/v1/tinvest/explanations/{explanationId}", authed(h.handleRemoveExplanation))
}

// writeError maps this package's own sentinels onto status codes and hands
// everything else to family.WriteError.
//
// THE SEPARATIONS BELOW ARE THE WHOLE POINT OF HAVING SENTINELS AT ALL. A
// refused token (400) and an unreachable broker (502) are opposite advice —
// paste a new token, or wait — and a client can only tell them apart by the
// code, since the text is prose. A connection that is switched off (409) is not
// a missing one (404). A broker account already imported (409) is not a
// malformed request (400). And a picked broker account this token cannot import
// (422) is not a refused token (400).
//
// THAT LAST ONE USED TO BE A 400 BESIDE THE REFUSED TOKEN, and the two share a
// path: POST /api/v1/tinvest/connections asks the broker for its account list
// afresh, so a list that changed since the wizard's token-check — an account
// closed, a token's access narrowed — refuses the create with a request that is
// perfectly well formed and a token that still works. Under one status code the
// client had to caption it as a refused token, telling the owner to check and
// re-issue a token that never stopped working; 422 says the other thing this
// call can mean, which is that the request was understood and cannot be carried
// out. Only this path can produce it (Service.CreateConnection), which is why
// only this path declares it in api/openapi.yaml.
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrTokenRejected):
		httpjson.Error(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrBrokerAccountNotImportable):
		httpjson.Error(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, ErrConnectionNotActive), errors.Is(err, ErrBrokerAccountAlreadyLinked),
		errors.Is(err, ErrRowAlreadyExplained), errors.Is(err, operation.ErrInconsistent):
		// THE JOURNAL'S OWN REFUSAL IS ANSWERED THE WAY THE JOURNAL ANSWERS IT.
		// An explanation hands the operation service a manual operation, and
		// when the engine will not replay the journal it would leave, that
		// service says so with ErrInconsistent — which the journal screen
		// answers with 409 and the engine's own sentence (see
		// operation.writeError). Without this branch it fell through to
		// family.WriteError's default and reached the owner as «internal
		// error», 500: the program telling them it had broken, when what
		// happened is that their history cannot hold that operation. Live, on
		// the very case this feature exists for — a redemption of 44 380,35
		// units offered while a transfer_out still held them.
		httpjson.Error(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrRowNotInLink), errors.Is(err, ErrExplanationNotFound), errors.Is(err, ErrLinkNotFound):
		// 404 rather than 400: each names something well formed that this
		// space does not have — a content key none of the link's rows carries,
		// an explanation or a link that is not here. A 400 would tell the
		// owner to fix the request they sent, which is not what is wrong.
		httpjson.Error(w, http.StatusNotFound, err.Error())
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

// nullableString is dateOrNull for a text the check may not have: an absent
// one is an explicit null, for the same reason — «the broker's passport was
// not obtained» is a statement, and an omitted key would read as the server
// having forgotten to answer.
func nullableString(s *string) nullable.Nullable[string] {
	if s == nil {
		return nullable.NewNullNullable[string]()
	}
	return nullable.NewNullableWithValue(*s)
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
		// The passport, field by field: each is null exactly when the check
		// recorded none — including every row of a run recorded before these
		// fields existed, whose jsonb carries no such keys and decodes to nil.
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
	// ONE VERDICT PER LINKED ACCOUNT, WALKED OFF THE LINKS THEMSELVES, so the
	// two lists are the same accounts in the same order by construction and an
	// account nobody checked cannot go missing: it is written out as
	// not_checked — the verdict it has — rather than being left for a reader to
	// notice was absent. Both lists take the account's name from the same link
	// row read once, so they cannot come to disagree about it.
	out.Reconciles = make([]apitypes.TinvestAccountReconcile, 0, len(v.Links))
	for _, l := range v.Links {
		item := apitypes.TinvestAccountReconcile{
			LinkId:            l.ID,
			AccountId:         l.AccountID,
			BrokerAccountName: l.BrokerAccountName,
			Status:            apitypes.TinvestReconcileStatus(ReconcileNotChecked),
			At:                nullable.NewNullNullable[time.Time](),
			Mismatches:        []apitypes.TinvestReconcileMismatch{},
			// Published on EVERY verdict, including not_checked and matched,
			// because it is a fact about the account rather than about the
			// comparison: a reader deciding what a cash difference means needs
			// it, and a reader seeing none needs to know there is none.
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
		row := apitypes.TinvestUnparsedOperation{
			Id: m.ID,
			// Which linked account the row is on: this list is per connection
			// and a connection may feed several, so without it a client could
			// not tell which link's endpoint to explain the row through.
			LinkId: m.LinkID,
			// What an explanation names this row by, and the reason it is
			// published at all: the broker's own operation id is documented to
			// change, so a client sending one back would be naming a row that
			// may no longer be there (see MirrorRow.ContentKey).
			ContentKey: m.ContentKey,
			OccurredAt: m.OccurredAt,
			OpType:     m.OpType,
			// Where the broker says it happened, and what this program can
			// call that: the code verbatim (empty when the broker sent none),
			// and the kind beside it. The kind is published even for an empty
			// code, where it says `unknown` — on THIS list that is the honest
			// answer, since the list exists to show what the broker sent and
			// an absent field would look like a field this screen forgot.
			ClassCode:       m.ClassCode,
			TradingModeKind: nullable.NewNullableWithValue(apitypes.TradingModeKind(tradingmode.Of(m.ClassCode))),
			Payment:         m.Payment.String(),
			Currency:        m.Currency,
			Description:     m.Description,
			Reason:          apitypes.TinvestUnparsedReason(m.UnparsedReason),
			// The refuser's own words, handed on as they were written. The
			// interface still chooses what to SAY from Reason alone; this is
			// shown, never read.
			Detail: m.UnparsedDetail,
			// The broker's own bytes, handed on rather than re-encoded: nothing
			// here computes from them, and re-modelling them would be a second
			// reading of a document this program deliberately keeps unread.
			Raw: m.Raw,
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
	// THE JOURNAL'S OWN READING OF ITS OWN REQUEST SHAPE. A second parser here
	// would be a second set of rules about what a date or a decimal is, and the
	// two would part company the first time either moved.
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

// handleRemoveExplanation deletes an explanation together with the manual
// operation it names — see Service.RemoveExplanation for why that is one
// action and not two.
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
