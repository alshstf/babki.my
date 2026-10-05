package tinvest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/logtest"
	"babki.my/babki/internal/platform/secretbox"
)

// Package tinvest, to reach the worker's unexported factories and reads and
// drive Work directly. Expected values are literals.

// A broker to sync against.

const (
	rpcOperations  = "OperationsService/GetOperationsByCursor"
	rpcPortfolio   = "OperationsService/GetPortfolio"
	rpcPositions   = "OperationsService/GetPositions"
	rpcInstrumentB = "InstrumentsService/GetInstrumentBy"
)

// brokerStub is an httptest server for the calls a sync makes, with replaceable
// status and body per rpc; it counts requests and remembers each "from", so tests
// can state what the broker was asked.
type brokerStub struct {
	srv *httptest.Server

	mu     sync.Mutex
	status map[string]int
	body   map[string]string
	calls  map[string]int
	froms  []string
	// opsByAccount answers operations per broker account; others get the
	// shared body.
	opsByAccount map[string]string

	// arrive runs before an operations page is answered, told the 1-based
	// call number: it holds two runs at one point, or switches an answer
	// between two calls.
	arrive func(call int)
}

func newBrokerStub(t *testing.T) *brokerStub {
	t.Helper()
	b := &brokerStub{
		status: map[string]int{},
		body: map[string]string{
			rpcOperations:  `{"hasNext":false,"nextCursor":"","items":[]}`,
			rpcPortfolio:   `{"positions":[]}`,
			rpcPositions:   `{"money":[],"blocked":[]}`,
			rpcInstrumentB: `{"instrument":{}}`,
		},
		calls:        map[string]int{},
		opsByAccount: map[string]string{},
	}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rpc := strings.TrimPrefix(r.URL.Path, rpcPathPrefix)
		raw := new(bytes.Buffer)
		if _, err := raw.ReadFrom(r.Body); err != nil {
			// t.Fatal must only be called from the test's own goroutine; this
			// handler runs on a server connection goroutine.
			t.Errorf("read request body: %v", err)
			return
		}
		_ = r.Body.Close()

		b.mu.Lock()
		b.calls[rpc]++
		call := b.calls[rpc]
		account := ""
		if rpc == rpcOperations {
			var req getOperationsByCursorRequest
			if err := json.Unmarshal(raw.Bytes(), &req); err != nil {
				t.Errorf("decode operations request: %v", err)
			}
			b.froms = append(b.froms, req.From)
			account = req.AccountID
		}
		arrive := b.arrive
		b.mu.Unlock()

		if rpc == rpcOperations && arrive != nil {
			arrive(call)
		}

		// Read AFTER the hook, so a hook that changes the answer changes THIS
		// answer rather than the next one.
		b.mu.Lock()
		status, body := b.status[rpc], b.body[rpc]
		if own, ok := b.opsByAccount[account]; ok && rpc == rpcOperations {
			body = own
		}
		b.mu.Unlock()

		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *brokerStub) answer(rpc string, status int, body string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status[rpc] = status
	b.body[rpc] = body
}

// answerAccount gives one broker account its own operations page.
func (b *brokerStub) answerAccount(brokerAccountID, page string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.opsByAccount[brokerAccountID] = page
}

func (b *brokerStub) callCount(rpc string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[rpc]
}

func (b *brokerStub) askedFroms() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.froms...)
}

// opJSON is one executed operation as the gateway sends it, written by
// hand so the client's real parsing runs.
func opJSON(id, opType, at string, units int64) string {
	return fmt.Sprintf(`{"id":%q,"type":%q,"state":"OPERATION_STATE_EXECUTED",`+
		`"date":%q,"payment":{"currency":"rub","units":"%d","nano":0},"quantity":"0",`+
		`"description":"Операция"}`, id, opType, at, units)
}

// depositJSON is one executed cash top-up.
func depositJSON(id string, at string, units int64) string {
	return opJSON(id, "OPERATION_TYPE_INPUT", at, units)
}

func operationsPage(items ...string) string {
	return `{"hasNext":false,"nextCursor":"","items":[` + strings.Join(items, ",") + `]}`
}

// opFixture is a testdata/ops document, put on the wire verbatim.
func opFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "ops", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(raw)
}

func moneyPositions(currency string, units int64) string {
	return fmt.Sprintf(`{"money":[{"currency":%q,"units":"%d","nano":0}],"blocked":[]}`, currency, units)
}

// The worker under test.

// testToken is the broker token the fixture seals into the connection. The
// worker must hand exactly this to its client factory.
const testToken = "t.a-read-only-token"

type workerFixture struct {
	fixture
	ops    *operation.Store
	broker *brokerStub
	logs   *logtest.Capture
	worker river.Worker[SyncArgs]

	mu     sync.Mutex
	tokens []string // every token the client factory was handed
}

func newWorkerFixture(t *testing.T) *workerFixture {
	t.Helper()
	return newWorkerFixtureWith(t, false)
}

// newWorkerFixtureWith builds the worker with or without the real
// corporate-actions registry over the same database.
func newWorkerFixtureWith(t *testing.T, withRegistry bool) *workerFixture {
	t.Helper()
	f := newFixture(t)

	box, err := secretbox.New(bytes.Repeat([]byte{7}, secretbox.KeySize))
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	if err := f.store.UpdateConnectionToken(f.ctx, f.spaceID, f.conn.ID,
		box.Seal([]byte(testToken)), "oken"); err != nil {
		t.Fatalf("UpdateConnectionToken: %v", err)
	}
	conn, err := f.store.ConnectionByID(f.ctx, f.spaceID, f.conn.ID)
	if err != nil {
		t.Fatalf("ConnectionByID: %v", err)
	}
	f.conn = conn

	wf := &workerFixture{fixture: f, ops: operation.NewStore(f.pool), broker: newBrokerStub(t), logs: &logtest.Capture{}}
	log := slog.New(wf.logs)

	instStore := instrument.NewStore(f.pool)
	newClient := func(token string) (*Client, error) {
		wf.mu.Lock()
		wf.tokens = append(wf.tokens, token)
		wf.mu.Unlock()
		return NewClient(wf.broker.srv.Client(), wf.broker.srv.URL, token, log), nil
	}
	newRebuilder := func() *Rebuilder {
		return NewRebuilder(f.store, NewResolver(f.store, instStore, log),
			operation.NewService(wf.ops), wf.ops, log)
	}
	reconciler := NewReconciler(f.store, wf.ops, account.NewStore(f.pool), instStore, nil, log)
	var registry registryAligner
	if withRegistry {
		registry = corporateaction.NewMaterializer(corporateaction.NewStore(f.pool),
			operation.NewService(wf.ops), instStore, nil, log)
	}
	wf.worker = NewSyncWorker(f.store, box, newClient, newRebuilder, registry, reconciler, log)
	return wf
}

// work drives one sync job, exactly as River would.
func (f *workerFixture) work(t *testing.T, trigger string) error {
	t.Helper()
	return f.worker.Work(f.ctx, &river.Job[SyncArgs]{
		JobRow: &rivertype.JobRow{ID: 1},
		Args:   SyncArgs{ConnectionID: f.conn.ID, Trigger: trigger},
	})
}

func (f *workerFixture) runs(t *testing.T) []SyncRun {
	t.Helper()
	runs, _, err := f.store.RunsByConnection(f.ctx, f.conn.ID, 50, 0)
	if err != nil {
		t.Fatalf("RunsByConnection: %v", err)
	}
	return runs
}

func (f *workerFixture) status(t *testing.T) ConnectionStatus {
	t.Helper()
	conn, err := f.store.ConnectionByID(f.ctx, f.spaceID, f.conn.ID)
	if err != nil {
		t.Fatalf("ConnectionByID: %v", err)
	}
	return conn.Status
}

func (f *workerFixture) journal(t *testing.T) []operation.Operation {
	t.Helper()
	ops, err := f.ops.ListBySource(f.ctx, f.spaceID, f.accountID, Source)
	if err != nil {
		t.Fatalf("ListBySource: %v", err)
	}
	return ops
}

func (f *workerFixture) mirrorRows(t *testing.T) []MirrorRow {
	t.Helper()
	rows, err := f.store.MirrorRowsByLink(f.ctx, f.link.ID)
	if err != nil {
		t.Fatalf("MirrorRowsByLink: %v", err)
	}
	return rows
}

// setOpenedOn writes the account's opening day onto the link. The fixture's own
// link carries none, which is the other half of the history floor.
func (f *workerFixture) setOpenedOn(t *testing.T, day string) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE tinvest_account_links SET opened_on = $2 WHERE id = $1`, f.link.ID, day); err != nil {
		t.Fatalf("set opened_on: %v", err)
	}
}

// The pipeline, end to end.

// One run carries the history into the mirror, the mirror into the journal,
// the broker's figure onto the account, and a run log entry; all four are
// asserted.
func TestSyncWorkerCarriesOneRunFromTheBrokerToTheJournal(t *testing.T) {
	f := newWorkerFixture(t)
	f.broker.answer(rpcOperations, http.StatusOK,
		operationsPage(depositJSON("op-1", "2026-03-14T07:30:15Z", 1000)))
	f.broker.answer(rpcPositions, http.StatusOK, moneyPositions("rub", 1000))
	f.broker.answer(rpcPortfolio, http.StatusOK,
		`{"positions":[],"totalAmountPortfolio":{"currency":"rub","units":"1000","nano":0}}`)

	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if got := f.tokens; len(got) != 1 || got[0] != testToken {
		t.Errorf("the client was built with %q, want exactly one client built with %q", got, testToken)
	}
	if rows := f.mirrorRows(t); len(rows) != 1 {
		t.Fatalf("mirror holds %d rows, want 1", len(rows))
	}

	journal := f.journal(t)
	if len(journal) != 1 {
		t.Fatalf("journal holds %d imported operations, want 1: %+v", len(journal), journal)
	}
	if journal[0].Type != operation.TypeDeposit || journal[0].AmountMinor != 100_000 || journal[0].Currency != "RUB" {
		t.Errorf("journal[0] = {%s %d %s}, want {deposit 100000 RUB}",
			journal[0].Type, journal[0].AmountMinor, journal[0].Currency)
	}

	acc, err := account.NewStore(f.pool).ByID(f.ctx, f.spaceID, f.accountID)
	if err != nil {
		t.Fatalf("account ByID: %v", err)
	}
	if acc.Balance == nil || acc.Balance.AmountMinor != 100_000 {
		t.Errorf("balance mark = %+v, want 100000 — the broker's own total for the account", acc.Balance)
	}

	runs := f.runs(t)
	if len(runs) != 1 {
		t.Fatalf("%d runs recorded, want 1", len(runs))
	}
	run := runs[0]
	if run.Status != RunOK || run.Trigger != TriggerSchedule || run.FinishedAt == nil {
		t.Errorf("run = {%s %s finished=%v}, want {ok schedule finished}", run.Status, run.Trigger, run.FinishedAt)
	}
	if run.ReadCount != 1 || run.AddedCount != 1 || run.DisappearedCount != 0 || run.UnparsedCount != 0 {
		t.Errorf("run counters = {read %d added %d gone %d unparsed %d}, want {1 1 0 0}",
			run.ReadCount, run.AddedCount, run.DisappearedCount, run.UnparsedCount)
	}
	if run.ReconcileStatus != ReconcileMatched || run.ReconciledAt == nil {
		t.Errorf("reconcile = {%s at %v}, want {matched, checked}", run.ReconcileStatus, run.ReconciledAt)
	}
	if run.Error != "" {
		t.Errorf("run error = %q, want empty", run.Error)
	}
}

// A second run over an unchanged broker changes nothing; the hourly schedule
// rests on it.
func TestSyncWorkerRunTwiceLeavesOneMirrorAndOneJournal(t *testing.T) {
	f := newWorkerFixture(t)
	f.broker.answer(rpcOperations, http.StatusOK,
		operationsPage(depositJSON("op-1", "2026-03-14T07:30:15Z", 1000)))
	f.broker.answer(rpcPositions, http.StatusOK, moneyPositions("rub", 1000))

	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("first Work: %v", err)
	}
	first := f.journal(t)
	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("second Work: %v", err)
	}
	second := f.journal(t)

	if len(f.mirrorRows(t)) != 1 {
		t.Errorf("mirror holds %d rows after two runs, want 1", len(f.mirrorRows(t)))
	}
	if len(second) != 1 {
		t.Fatalf("journal holds %d operations after two runs, want 1", len(second))
	}
	// The same row id, not merely the same count.
	if first[0].ID != second[0].ID {
		t.Errorf("the journal operation was rewritten: %s became %s", first[0].ID, second[0].ID)
	}
	if runs := f.runs(t); len(runs) != 2 {
		t.Errorf("%d runs recorded, want 2", len(runs))
	}
}

// The projection is rebuilt once, over every link, after every mirror: the two
// legs of a move between the owner's accounts sit under two links, and a
// per-link rebuild would pair nothing.
func TestSyncWorkerRebuildsTheWholeConnectionSoTransfersPair(t *testing.T) {
	f := newWorkerFixture(t)
	second := f.secondLink(t)
	f.broker.answerAccount(f.link.BrokerAccountID,
		operationsPage(opFixture(t, "buy.json"), opFixture(t, "trans_bs_bs_out.json")))
	f.broker.answerAccount(second.BrokerAccountID,
		operationsPage(opFixture(t, "trans_bs_bs_in.json")))
	f.broker.answer(rpcInstrumentB, http.StatusOK, string(readFixture(t, "instrument.json")))

	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("Work: %v", err)
	}

	var out, in *operation.Operation
	departures := f.journal(t)
	for i := range departures {
		if departures[i].Type == operation.TypeTransferOut {
			out = &departures[i]
		}
	}
	arrivals, err := f.ops.ListBySource(f.ctx, f.spaceID, second.AccountID, Source)
	if err != nil {
		t.Fatalf("ListBySource: %v", err)
	}
	for i, o := range arrivals {
		if o.Type == operation.TypeTransferIn {
			in = &arrivals[i]
		}
	}
	if out == nil || in == nil {
		t.Fatalf("legs found: out=%v in=%v, want both", out, in)
	}
	if out.TransferGroupID == nil || in.TransferGroupID == nil || *out.TransferGroupID != *in.TransferGroupID {
		t.Fatalf("legs carry groups %v and %v, want one group on both — they are one event",
			out.TransferGroupID, in.TransferGroupID)
	}
}

// A run's unparsed count is its own account's, not the connection's. One
// link gets an unknown operation type, the other nothing.
func TestSyncWorkerCountsUnparsedRowsPerLinkAndNotPerConnection(t *testing.T) {
	f := newWorkerFixture(t)
	second := f.secondLink(t)
	f.broker.answerAccount(second.BrokerAccountID,
		operationsPage(opJSON("op-x", "OPERATION_TYPE_MYSTERY", "2026-03-14T07:30:15Z", -100)))

	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("Work: %v", err)
	}

	byLink := map[uuid.UUID]SyncRun{}
	for _, r := range f.runs(t) {
		byLink[r.LinkID] = r
	}
	if len(byLink) != 2 {
		t.Fatalf("%d links have a run, want 2", len(byLink))
	}
	if got := byLink[f.link.ID].UnparsedCount; got != 0 {
		t.Errorf("the account with nothing wrong reports %d unparsed operations, want 0", got)
	}
	if got := byLink[second.ID].UnparsedCount; got != 1 {
		t.Errorf("the account with an unreadable operation reports %d unparsed, want 1", got)
	}
	// An operation this program cannot read is not a failure of the run: it is
	// recorded, shown to the owner, and everything else still goes through.
	if got := byLink[second.ID].Status; got != RunOK {
		t.Errorf("run status = %q, want %q — an unreadable operation is reported, not fatal", got, RunOK)
	}
}

// How far back a run reads.

// Every run reads the whole history from the account's opening: the broker
// rewrites old operations, and SyncMirror marks what it does not find as gone.
// Asserted on the second run, when a last-sync time exists to narrow by.
func TestSyncWorkerAsksForTheWholeHistoryOnEveryRun(t *testing.T) {
	f := newWorkerFixture(t)
	f.setOpenedOn(t, "2021-03-15")
	f.broker.answer(rpcOperations, http.StatusOK,
		operationsPage(depositJSON("op-1", "2026-03-14T07:30:15Z", 1000)))
	f.broker.answer(rpcPositions, http.StatusOK, moneyPositions("rub", 1000))

	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("first Work: %v", err)
	}
	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("second Work: %v", err)
	}

	froms := f.broker.askedFroms()
	if len(froms) != 2 {
		t.Fatalf("the broker was asked for operations %d times, want 2", len(froms))
	}
	for i, from := range froms {
		if from != "2021-03-15T00:00:00Z" {
			t.Errorf("run %d asked from %q, want %q — the account's opening day, every time",
				i+1, from, "2021-03-15T00:00:00Z")
		}
	}
}

// A link with no opening day still has to start somewhere, and the floor is the
// year the broker's API opened.
func TestSyncWorkerFallsBackToTheYearTheBrokersAPIOpened(t *testing.T) {
	f := newWorkerFixture(t)

	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("Work: %v", err)
	}

	froms := f.broker.askedFroms()
	if len(froms) != 1 || froms[0] != "2016-01-01T00:00:00Z" {
		t.Errorf("asked from %q, want %q", froms, "2016-01-01T00:00:00Z")
	}
}

// What stops a run, and what it does about it.

// A switched-off connection: no broker call, no run, no error, a Debug
// line.
func TestSyncWorkerLeavesAConnectionThatIsSwitchedOffAlone(t *testing.T) {
	f := newWorkerFixture(t)
	if err := f.store.UpdateConnectionStatus(f.ctx, f.conn.ID, StatusDisabled); err != nil {
		t.Fatalf("UpdateConnectionStatus: %v", err)
	}

	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("Work returned %v, want nil — a switched-off connection is not a failure", err)
	}
	if n := f.broker.callCount(rpcOperations); n != 0 {
		t.Errorf("the broker was called %d times for a switched-off connection, want 0", n)
	}
	if runs := f.runs(t); len(runs) != 0 {
		t.Errorf("%d runs recorded for a switched-off connection, want 0", len(runs))
	}
	logtest.AssertOne(t, f.logs, connectionNotActiveMessage, slog.LevelDebug, "disabled")
}

// A refused token parks the connection, records the run failed and returns
// nil, so the queue stops.
func TestSyncWorkerParksAConnectionWhoseTokenTheBrokerRefuses(t *testing.T) {
	f := newWorkerFixture(t)
	f.broker.answer(rpcOperations, http.StatusUnauthorized,
		`{"code":16,"message":"authentication token is missing or invalid","description":"40003"}`)

	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("Work returned %v, want nil — retrying cannot mend a revoked token", err)
	}
	if got := f.status(t); got != StatusTokenRevoked {
		t.Errorf("connection status = %q, want %q", got, StatusTokenRevoked)
	}
	runs := f.runs(t)
	if len(runs) != 1 {
		t.Fatalf("%d runs recorded, want 1", len(runs))
	}
	if runs[0].Status != RunFailed || runs[0].FinishedAt == nil {
		t.Errorf("run = {%s finished=%v}, want {failed, finished}", runs[0].Status, runs[0].FinishedAt)
	}
	if runs[0].Error == "" {
		t.Error("the failed run carries no error text; a run log that cannot say why is no log")
	}
	if runs[0].ReconcileStatus != ReconcileNotChecked {
		t.Errorf("reconcile = %q, want %q — nothing was checked", runs[0].ReconcileStatus, ReconcileNotChecked)
	}
}

// Any other failure comes back out of Work for River to retry, and the
// connection keeps its status.
func TestSyncWorkerReturnsAnyOtherFailureSoTheQueueRetries(t *testing.T) {
	f := newWorkerFixture(t)
	f.broker.answer(rpcOperations, http.StatusInternalServerError, `{"code":13,"message":"internal"}`)

	err := f.work(t, "schedule")
	if err == nil {
		t.Fatal("Work returned nil for a broker failure; the queue would never retry it")
	}
	if errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Work returned %v, which is the token sentinel; this test would then prove nothing", err)
	}
	if got := f.status(t); got != StatusActive {
		t.Errorf("connection status = %q, want %q — a 500 says nothing about the token", got, StatusActive)
	}
	runs := f.runs(t)
	if len(runs) != 1 || runs[0].Status != RunFailed || runs[0].FinishedAt == nil {
		t.Fatalf("runs = %+v, want exactly one finished failed run", runs)
	}
}

// Every started run is closed, including ones that succeeded before a later
// link failed; "running" forever means a crash.
func TestSyncWorkerClosesEveryRunItStartedWhenALaterLinkFails(t *testing.T) {
	f := newWorkerFixture(t)
	second := f.secondLink(t)
	// The first link's page is served, then the failure is switched on.
	f.broker.mu.Lock()
	f.broker.arrive = func(call int) {
		if call == 2 {
			f.broker.answer(rpcOperations, http.StatusInternalServerError, `{"code":13,"message":"internal"}`)
		}
	}
	f.broker.mu.Unlock()

	if err := f.work(t, "schedule"); err == nil {
		t.Fatal("Work returned nil though the second link's read failed")
	}

	runs := f.runs(t)
	if len(runs) != 2 {
		t.Fatalf("%d runs recorded, want one per link (2)", len(runs))
	}
	links := map[uuid.UUID]bool{}
	for _, r := range runs {
		links[r.LinkID] = true
		if r.Status != RunFailed || r.FinishedAt == nil {
			t.Errorf("run of link %s = {%s finished=%v}, want {failed, finished}", r.LinkID, r.Status, r.FinishedAt)
		}
	}
	// Named rather than counted: two runs of the SAME link would satisfy the
	// count above while saying nothing about the link that failed.
	if !links[f.link.ID] || !links[second.ID] {
		t.Errorf("runs cover links %v, want one for %s and one for %s", links, f.link.ID, second.ID)
	}
}

// A run that did its work but could not close its log entry still writes its
// summary. The log row is deleted while the run waits at the broker, so the
// close must fail.
func TestSyncWorkerStillSummarisesARunWhoseLogEntryCannotBeClosed(t *testing.T) {
	f := newWorkerFixture(t)
	f.broker.answer(rpcOperations, http.StatusOK,
		operationsPage(depositJSON("op-1", "2026-03-14T07:30:15Z", 1000)))
	f.broker.answer(rpcPositions, http.StatusOK, moneyPositions("rub", 1000))
	f.broker.mu.Lock()
	f.broker.arrive = func(int) {
		if _, err := f.pool.Exec(f.ctx, `DELETE FROM tinvest_sync_runs`); err != nil {
			// t.Fatal must only be called from the test's own goroutine.
			t.Errorf("delete the run rows: %v", err)
		}
	}
	f.broker.mu.Unlock()

	if err := f.work(t, "schedule"); err == nil {
		t.Fatal("Work returned nil though the run's log entry could not be closed")
	}

	rec := logtest.AssertOne(t, f.logs, "tinvest: a sync run finished", slog.LevelInfo, f.conn.ID.String())
	// The figures too: a summary that survived but reported nothing would be
	// the same silence in a different shape.
	logtest.AttrIs(t, rec, "read", "1")
	logtest.AttrIs(t, rec, "added", "1")
}

// A failed run records the unparsed count it took, not a zero that would read
// as "counted, none". The run stops at reconciliation, after the rebuild marked
// one unknown operation.
func TestSyncWorkerRecordsTheUnparsedCountItTookOnARunThatFailed(t *testing.T) {
	f := newWorkerFixture(t)
	f.broker.answer(rpcOperations, http.StatusOK,
		operationsPage(opJSON("op-x", "OPERATION_TYPE_MYSTERY", "2026-03-14T07:30:15Z", -100)))
	f.broker.answer(rpcPortfolio, http.StatusInternalServerError, `{"code":13,"message":"internal"}`)

	if err := f.work(t, "schedule"); err == nil {
		t.Fatal("Work returned nil though the reconciliation failed")
	}

	runs := f.runs(t)
	if len(runs) != 1 {
		t.Fatalf("%d runs recorded, want 1", len(runs))
	}
	if runs[0].Status != RunFailed || runs[0].FinishedAt == nil {
		t.Fatalf("run = {%s finished=%v}, want {failed, finished}", runs[0].Status, runs[0].FinishedAt)
	}
	if got := runs[0].UnparsedCount; got != 1 {
		t.Errorf("the failed run reports %d unreadable operations, want 1 — the mirror holds one, "+
			"and a zero here would be a measurement nobody made", got)
	}
	// For contrast, the reconciliation says "not checked".
	if runs[0].ReconcileStatus != ReconcileNotChecked {
		t.Errorf("reconcile = %q, want %q", runs[0].ReconcileStatus, ReconcileNotChecked)
	}
}

// A trigger the run log cannot store is refused before a run opens, and not
// retried: the arguments never change. One line, then the job is let go.
func TestSyncWorkerDropsAJobNamingATriggerTheRunLogCannotStore(t *testing.T) {
	f := newWorkerFixture(t)

	if err := f.work(t, "whenever"); err != nil {
		t.Fatalf("Work returned %v, want nil — no retry can mend an argument that cannot change", err)
	}
	if n := f.broker.callCount(rpcOperations); n != 0 {
		t.Errorf("the broker was called %d times on a job that could not be logged, want 0", n)
	}
	if runs := f.runs(t); len(runs) != 0 {
		t.Errorf("%d runs recorded, want 0", len(runs))
	}
	// That line is visible at the production level and names the word: it is
	// the only record.
	logtest.AssertOne(t, f.logs,
		"tinvest: a sync job names a trigger the run log cannot store, dropping it",
		slog.LevelError, `unknown sync trigger: "whenever"`)
}

// A connection with no accounts picked yet: nothing to sync, no broker
// call.
func TestSyncWorkerSaysThereIsNothingToSyncWithoutLinkedAccounts(t *testing.T) {
	f := newWorkerFixture(t)
	if _, err := f.pool.Exec(f.ctx,
		`DELETE FROM tinvest_account_links WHERE connection_id = $1`, f.conn.ID); err != nil {
		t.Fatalf("remove the link: %v", err)
	}

	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("Work returned %v, want nil", err)
	}
	if n := f.broker.callCount(rpcOperations); n != 0 {
		t.Errorf("the broker was called %d times for a connection with no linked accounts, want 0", n)
	}
	logtest.AssertOne(t, f.logs, noLinksMessage, slog.LevelDebug, f.conn.ID.String())
}

// The owner may delete a connection while a job for it is already queued. That
// is not a failure of anything and must not be retried.
func TestSyncWorkerSaysNothingIsThereWhenTheConnectionIsGone(t *testing.T) {
	f := newWorkerFixture(t)
	if err := f.store.DeleteConnection(f.ctx, f.spaceID, f.conn.ID); err != nil {
		t.Fatalf("DeleteConnection: %v", err)
	}

	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("Work returned %v, want nil — a deleted connection is nothing to retry", err)
	}
	logtest.AssertOne(t, f.logs, connectionGoneMessage, slog.LevelDebug, f.conn.ID.String())
}

// A cancelled pass (shutdown) is logged as routine, not Error, in both
// workers; the rule lives in a helper each must remember to call.
func TestACancelledPassIsLoggedAsRoutineAndNotAsAFailure(t *testing.T) {
	f := newWorkerFixture(t)
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()

	dispatchLogs := &logtest.Capture{}
	dispatcher := NewDispatchWorker(f.store, &recordingInserter{}, slog.New(dispatchLogs))
	if err := dispatcher.Work(ctx, &river.Job[SyncDispatchArgs]{JobRow: &rivertype.JobRow{ID: 1}}); err == nil {
		t.Fatal("the dispatcher returned nil though its context was cancelled")
	}
	logtest.AssertOne(t, dispatchLogs,
		"tinvest: list the active connections failed", slog.LevelDebug, "context canceled")

	if err := f.worker.Work(ctx, &river.Job[SyncArgs]{
		JobRow: &rivertype.JobRow{ID: 1},
		Args:   SyncArgs{ConnectionID: f.conn.ID, Trigger: "schedule"},
	}); err == nil {
		t.Fatal("the sync worker returned nil though its context was cancelled")
	}
	logtest.AssertOne(t, f.logs,
		"tinvest: read the connection to sync failed", slog.LevelDebug, "context canceled")
}

// Two runs of one connection at once.

// Two simultaneous runs leave one mirror and one journal. SyncMirror's lock on
// the connection is what ensures it. The journal's dedup index only catches runs
// that agree on the mirror rows; two runs that each inserted the operation give
// entries different names (with the lock removed: two mirror rows and two
// operations). One run may fail; no duplicate may appear. Both runs are held at
// the broker until both arrive.
func TestTwoSimultaneousRunsOfOneConnectionLeaveOneMirrorAndOneJournal(t *testing.T) {
	f := newWorkerFixture(t)
	f.broker.answer(rpcOperations, http.StatusOK,
		operationsPage(depositJSON("op-1", "2026-03-14T07:30:15Z", 1000)))
	f.broker.answer(rpcPositions, http.StatusOK, moneyPositions("rub", 1000))

	var (
		gate    = make(chan struct{})
		arrived sync.WaitGroup
	)
	arrived.Add(2)
	var once sync.Once
	f.broker.mu.Lock()
	f.broker.arrive = func(int) {
		arrived.Done()
		arrived.Wait()
		once.Do(func() { close(gate) })
		<-gate
	}
	f.broker.mu.Unlock()

	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { done <- f.work(t, "schedule") }()
	}
	succeeded := 0
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err == nil {
				succeeded++
			}
		case <-time.After(60 * time.Second):
			t.Fatal("a run never finished")
		}
	}
	if succeeded == 0 {
		t.Fatal("neither of the two simultaneous runs succeeded")
	}

	if rows := f.mirrorRows(t); len(rows) != 1 {
		t.Errorf("mirror holds %d rows after two simultaneous runs of one operation, want 1", len(rows))
	}
	if journal := f.journal(t); len(journal) != 1 {
		t.Errorf("journal holds %d imported operations after two simultaneous runs, want 1: %+v",
			len(journal), journal)
	}
	for _, r := range f.runs(t) {
		if r.FinishedAt == nil {
			t.Errorf("run %s was left open", r.ID)
		}
	}
}

// The dispatcher.

// recordingInserter remembers what was queued instead of queueing it.
type recordingInserter struct {
	args []SyncArgs
	opts []*river.InsertOpts
	err  error
}

func (r *recordingInserter) Insert(_ context.Context, args river.JobArgs, opts *river.InsertOpts) (
	*rivertype.JobInsertResult, error,
) {
	if r.err != nil {
		return nil, r.err
	}
	r.args = append(r.args, args.(SyncArgs))
	r.opts = append(r.opts, opts)
	return &rivertype.JobInsertResult{Job: &rivertype.JobRow{ID: int64(len(r.args))}}, nil
}

// One job per active connection; a refused token is not asked again hourly.
func TestDispatchWorkerQueuesOneJobForEachActiveConnection(t *testing.T) {
	f := newFixture(t)
	parked, err := f.store.CreateConnection(f.ctx, f.spaceID, []byte("sealed"), "1234", StatusActive)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if err := f.store.UpdateConnectionStatus(f.ctx, parked.ID, StatusTokenRevoked); err != nil {
		t.Fatalf("UpdateConnectionStatus: %v", err)
	}

	inserter := &recordingInserter{}
	w := NewDispatchWorker(f.store, inserter, slog.New(&logtest.Capture{}))
	if err := w.Work(f.ctx, &river.Job[SyncDispatchArgs]{JobRow: &rivertype.JobRow{ID: 1}}); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if len(inserter.args) != 1 {
		t.Fatalf("%d jobs queued, want 1 (the active connection only): %+v", len(inserter.args), inserter.args)
	}
	if inserter.args[0] != (SyncArgs{ConnectionID: f.conn.ID, Trigger: "schedule"}) {
		t.Errorf("queued %+v, want {%s schedule}", inserter.args[0], f.conn.ID)
	}
	// The whole options value, written out: ByState is what keeps completed jobs
	// from blocking the next hour (River's default includes completed), and
	// comparing with SyncInsertOpts() itself would move with it.
	want := &river.InsertOpts{MaxAttempts: 7, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
		rivertype.JobStateAvailable,
		rivertype.JobStatePending,
		rivertype.JobStateRetryable,
		rivertype.JobStateRunning,
		rivertype.JobStateScheduled,
	}}}
	if opts := inserter.opts[0]; !reflect.DeepEqual(opts, want) {
		t.Errorf("queued with opts %+v, want %+v — the very options the manual button uses", opts, want)
	}
}

// Nothing to do is routine — an instance where nobody has connected a broker is
// the ordinary state — so it is a Debug line and not a failure.
func TestDispatchWorkerSaysThereIsNothingToSyncWhenNoConnectionIsActive(t *testing.T) {
	f := newFixture(t)
	if err := f.store.UpdateConnectionStatus(f.ctx, f.conn.ID, StatusDisabled); err != nil {
		t.Fatalf("UpdateConnectionStatus: %v", err)
	}
	logs := &logtest.Capture{}
	inserter := &recordingInserter{}

	w := NewDispatchWorker(f.store, inserter, slog.New(logs))
	if err := w.Work(f.ctx, &river.Job[SyncDispatchArgs]{JobRow: &rivertype.JobRow{ID: 1}}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(inserter.args) != 0 {
		t.Errorf("%d jobs queued, want 0", len(inserter.args))
	}
	rec := logtest.AssertOne(t, logs, nothingToSyncMessage, slog.LevelDebug, "connections")
	logtest.AttrIs(t, rec, "connections", "0")
}

// A queue that will not take the job is this worker's own failure, so it comes
// back out and River retries the dispatch.
func TestDispatchWorkerReturnsAQueueThatWillNotTakeTheJob(t *testing.T) {
	f := newFixture(t)
	boom := errors.New("queue is down")
	w := NewDispatchWorker(f.store, &recordingInserter{err: boom}, slog.New(&logtest.Capture{}))

	if err := w.Work(f.ctx, &river.Job[SyncDispatchArgs]{JobRow: &rivertype.JobRow{ID: 1}}); !errors.Is(err, boom) {
		t.Fatalf("Work returned %v, want %v", err, boom)
	}
}

// Uniqueness, against the real queue.

// Uniqueness is per connection, not per trigger: the `river:"unique"` tag on
// ConnectionID puts the schedule's job and the button's in one class. Against
// the real queue, since the tag's effect lives in River.
func TestSyncJobsAreUniquePerConnectionWhateverTriggeredThem(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	client, err := river.NewClient(riverpgxv5.New(f.pool), &river.Config{})
	if err != nil {
		t.Fatalf("river.NewClient: %v", err)
	}

	first, err := EnqueueSync(ctx, client, f.conn.ID, TriggerSchedule)
	if err != nil {
		t.Fatalf("EnqueueSync (schedule): %v", err)
	}
	if first.UniqueSkippedAsDuplicate {
		t.Fatal("the first job was skipped as a duplicate of nothing")
	}

	second, err := EnqueueSync(ctx, client, f.conn.ID, TriggerManual)
	if err != nil {
		t.Fatalf("EnqueueSync (manual): %v", err)
	}
	if !second.UniqueSkippedAsDuplicate {
		t.Error("a manual run was queued alongside a scheduled one for the same connection")
	}

	other, err := EnqueueSync(ctx, client, uuid.New(), TriggerSchedule)
	if err != nil {
		t.Fatalf("EnqueueSync (another connection): %v", err)
	}
	if other.UniqueSkippedAsDuplicate {
		t.Error("another connection's sync was refused as a duplicate; uniqueness is not per connection")
	}
}

// doneWorker is a sync worker that does nothing and succeeds, so that a job can
// be carried through the real queue to `completed` without a broker anywhere.
type doneWorker struct {
	river.WorkerDefaults[SyncArgs]
	done chan struct{}
}

func (w *doneWorker) Work(context.Context, *river.Job[SyncArgs]) error {
	close(w.done)
	return nil
}

// The next hour's sync is queued after the first finishes: River's default
// unique states include completed, which would block it until the cleaner ran.
// Taken through the real queue to completion.
func TestASecondSyncIsQueuedOnceTheFirstHasFinished(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx

	worker := &doneWorker{done: make(chan struct{})}
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	client, err := river.NewClient(riverpgxv5.New(f.pool), &river.Config{
		Logger:  slog.New(&logtest.Capture{}),
		Workers: workers,
		Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
	})
	if err != nil {
		t.Fatalf("river.NewClient: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	}()

	if _, err := EnqueueSync(ctx, client, f.conn.ID, TriggerSchedule); err != nil {
		t.Fatalf("EnqueueSync (first): %v", err)
	}
	select {
	case <-worker.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the first sync job never ran")
	}
	// Worked is not yet finished: River records the completion after Work
	// returns, so the row's state is what has to be waited for, not the call.
	waitForJobState(t, f, "completed")

	again, err := EnqueueSync(ctx, client, f.conn.ID, TriggerSchedule)
	if err != nil {
		t.Fatalf("EnqueueSync (next hour): %v", err)
	}
	if again.UniqueSkippedAsDuplicate {
		t.Fatal("the next hour's sync was refused as a duplicate of a job that had already finished; " +
			"this connection would never be synced again")
	}
}

// waitForJobState waits until some sync job of this test's database reaches the
// given state.
func waitForJobState(t *testing.T, f fixture, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var n int
		if err := f.pool.QueryRow(f.ctx,
			`SELECT count(*) FROM river_job WHERE kind = $1 AND state = $2`,
			SyncArgs{}.Kind(), want).Scan(&n); err != nil {
			t.Fatalf("read river_job: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no sync job reached state %q", want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Shapes the rest of the application depends on.

func TestJobKindsAreNamespacedToThisModule(t *testing.T) {
	if got := (SyncArgs{}).Kind(); got != "tinvest.sync" {
		t.Errorf("SyncArgs.Kind() = %q, want %q", got, "tinvest.sync")
	}
	if got := (SyncDispatchArgs{}).Kind(); got != "tinvest.sync_dispatch" {
		t.Errorf("SyncDispatchArgs.Kind() = %q, want %q", got, "tinvest.sync_dispatch")
	}
}

// The job is allowed far longer than River's one-minute default: a first run
// walks an account's entire history and then rebuilds the projection over it.
func TestSyncWorkerAsksForMoreThanTheDefaultMinute(t *testing.T) {
	f := newWorkerFixture(t)
	got := f.worker.Timeout(&river.Job[SyncArgs]{JobRow: &rivertype.JobRow{ID: 1}})
	if got != 15*time.Minute {
		t.Errorf("Timeout = %s, want 15m0s", got)
	}
}

// jobInserter must stay the real client's method.
var _ jobInserter = (*river.Client[pgx.Tx])(nil)

// A token that will not decrypt (the key changed, e.g. a backup restored onto
// a host with a new BABKI_ENCRYPTION_KEY) parks the connection like a revoked
// token: pasting a token is the remedy either way, and that state is where the
// screen offers it.
func TestSyncWorkerParksAConnectionWhoseTokenWillNotDecrypt(t *testing.T) {
	f := newWorkerFixture(t)

	other, err := secretbox.New(bytes.Repeat([]byte{9}, secretbox.KeySize))
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	if err := f.store.UpdateConnectionToken(f.ctx, f.spaceID, f.conn.ID,
		other.Seal([]byte(testToken)), "oken"); err != nil {
		t.Fatalf("reseal the token under a key the worker does not hold: %v", err)
	}

	if err := f.work(t, "schedule"); err != nil {
		t.Fatalf("Work returned %v, want nil — no retry can recover a key the process does not have", err)
	}
	if got := f.status(t); got != StatusTokenRevoked {
		t.Errorf("connection status = %q, want %q — otherwise nothing on any screen says the import stopped",
			got, StatusTokenRevoked)
	}
}
