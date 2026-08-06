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
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/secretbox"
)

// This file is package tinvest (not tinvest_test) so it can reach the worker's
// own unexported types — clientFactory, rebuilderFactory, the store's
// worker-only reads — and so that a test can drive Work directly rather than
// through a running queue.
//
// EVERY EXPECTED VALUE IS A LITERAL, for the reason the other files in this
// package state: an expectation derived from the implementation moves with it
// and stays green through a change of rule.

// -------------------------------------------------------------------------
// capturing log records whole
// -------------------------------------------------------------------------

// logCapture is an slog.Handler that keeps every record, so a test can assert
// the LEVEL a line was written at rather than merely that some line was
// written. A substring match against a buffer cannot tell a Warn from a Debug,
// and a Debug is invisible at the level this application runs at — so a test
// that only greps for the text goes on passing after the line has been demoted
// into silence. (The same handler exists in internal/marketdata for the same
// reason; it lives in that package's own test files and cannot be imported.)
//
// Enabled answers true for every level on purpose: the point is to observe the
// level, and a handler that filtered would make a demoted line disappear
// instead of being reported at the wrong level.
type logCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r.Clone())
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

func (c *logCapture) all() []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]slog.Record, len(c.records))
	copy(out, c.records)
	return out
}

// assertOneRecordAt fails unless exactly one captured record carries msg, that
// record was written at want, and its attributes name cause. The level is
// compared for EQUALITY rather than "at least want": a demotion buries the line
// under the production level and a promotion makes it read as news of a kind it
// is not, and "at least" would pass for half of those.
//
// It returns the record it matched, for callers that then have something to say
// about a particular attribute — see assertAttrIs, and why a substring is not
// enough for a number.
func assertOneRecordAt(t *testing.T, c *logCapture, msg string, want slog.Level, cause string) slog.Record {
	t.Helper()
	records := c.all()
	var matched []slog.Record
	for _, r := range records {
		if r.Message == msg {
			matched = append(matched, r)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("%d records say %q, want exactly 1; everything captured: %s",
			len(matched), msg, describeRecords(records))
	}
	if matched[0].Level != want {
		t.Fatalf("%q was logged at %s, want %s; everything captured: %s",
			msg, matched[0].Level, want, describeRecords(records))
	}
	if got := attrsOf(matched[0]); !strings.Contains(got, cause) {
		t.Fatalf("%q carried %s, which does not name the cause %q — a line nobody can act on is barely better than silence",
			msg, got, cause)
	}
	return matched[0]
}

// assertAttrIs fails unless the record carries key with exactly want.
//
// IT IS NOT assertOneRecordAt's SUBSTRING, and the difference matters wherever
// the value is a number: "0" is a substring of "connections=10" as much as of
// "connections=0", so a substring check on a count proves nothing at all. (This
// project's own rules name that trap; it had already been walked into here.)
func assertAttrIs(t *testing.T, r slog.Record, key, want string) {
	t.Helper()
	var got string
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			got, found = a.Value.String(), true
			return false
		}
		return true
	})
	if !found {
		t.Fatalf("%q carries no %q attribute at all; it has %s", r.Message, key, attrsOf(r))
	}
	if got != want {
		t.Fatalf("%q says %s=%q, want %q", r.Message, key, got, want)
	}
}

func describeRecords(records []slog.Record) string {
	if len(records) == 0 {
		return "(nothing at all was logged)"
	}
	var b strings.Builder
	for i, r := range records {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(r.Level.String())
		b.WriteString(" ")
		b.WriteString(r.Message)
	}
	return b.String()
}

func attrsOf(r slog.Record) string {
	var b strings.Builder
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(a.Key)
		b.WriteString("=")
		b.WriteString(a.Value.String())
		b.WriteString(" ")
		return true
	})
	return b.String()
}

// -------------------------------------------------------------------------
// a broker to sync against
// -------------------------------------------------------------------------

const (
	rpcOperations  = "OperationsService/GetOperationsByCursor"
	rpcPortfolio   = "OperationsService/GetPortfolio"
	rpcPositions   = "OperationsService/GetPositions"
	rpcInstrumentB = "InstrumentsService/GetInstrumentBy"
)

// brokerStub is an httptest server answering the four calls one sync run can
// make. Each rpc has a canned status and body a test may replace, and every
// request is counted and its "from" remembered — which is how the tests below
// state what the broker was ASKED, not only what it answered.
type brokerStub struct {
	srv *httptest.Server

	mu     sync.Mutex
	status map[string]int
	body   map[string]string
	calls  map[string]int
	froms  []string
	// opsByAccount answers the operations call per broker account, for the
	// tests where the two links must not see the same history. An account with
	// no entry gets the shared body.
	opsByAccount map[string]string

	// arrive, when set, runs before an operations page is answered and is told
	// which call this is (1-based). It is how the concurrency test holds two
	// runs at the same point instead of hoping they overlap, and how the
	// second-link test switches the answer on between two calls of one run.
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

// opJSON is one executed operation as the REST gateway sends it — written out
// by hand rather than marshalled from OperationItem, so that the worker's path
// runs through the client's real parsing.
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

// opFixture is one of testdata/ops's documents, verbatim, for a page. The
// fixtures are what the gateway sends, so they go onto the wire unchanged
// rather than through a Go type and back.
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

// -------------------------------------------------------------------------
// the worker under test
// -------------------------------------------------------------------------

// testToken is the broker token the fixture seals into the connection. The
// worker must hand exactly this to its client factory.
const testToken = "t.a-read-only-token"

type workerFixture struct {
	fixture
	ops    *operation.Store
	broker *brokerStub
	logs   *logCapture
	worker river.Worker[SyncArgs]

	mu     sync.Mutex
	tokens []string // every token the client factory was handed
}

func newWorkerFixture(t *testing.T) *workerFixture {
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

	wf := &workerFixture{fixture: f, ops: operation.NewStore(f.pool), broker: newBrokerStub(t), logs: &logCapture{}}
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
	reconciler := NewReconciler(f.store, wf.ops, account.NewStore(f.pool), log)
	wf.worker = NewSyncWorker(f.store, box, newClient, newRebuilder, reconciler, log)
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

// -------------------------------------------------------------------------
// the pipeline, end to end
// -------------------------------------------------------------------------

// One run carries the broker's history into the mirror, the mirror into the
// journal, and the broker's own ruble figure onto the account — and says so in
// the run log. It is the whole point of this worker, so it is asserted at every
// one of those four places rather than at the last one.
func TestSyncWorkerCarriesOneRunFromTheBrokerToTheJournal(t *testing.T) {
	f := newWorkerFixture(t)
	f.broker.answer(rpcOperations, http.StatusOK,
		operationsPage(depositJSON("op-1", "2026-03-14T07:30:15Z", 1000)))
	f.broker.answer(rpcPositions, http.StatusOK, moneyPositions("rub", 1000))

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
		t.Errorf("balance mark = %+v, want 100000 — the broker's own rubles", acc.Balance)
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

// A second run over an unchanged broker changes nothing and adds nothing: the
// mirror confirms its rows and the projection asks the journal for no
// difference at all. This is the property the hourly schedule rests on.
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
	// The row's own id, not merely the count: a rebuild that deleted the
	// operation and wrote it back would leave one row here too, and would break
	// every reference to it.
	if first[0].ID != second[0].ID {
		t.Errorf("the journal operation was rewritten: %s became %s", first[0].ID, second[0].ID)
	}
	if runs := f.runs(t); len(runs) != 2 {
		t.Errorf("%d runs recorded, want 2", len(runs))
	}
}

// THE PROJECTION IS REBUILT ONCE, OVER EVERY LINK, AFTER EVERY MIRROR IS UP TO
// DATE — and this is what that order is for. A move between two accounts of one
// connection is two mirror rows filed under two different links, and a rebuild
// that ran per link would see one leg at a time and pair nothing: the owner
// would get two unrelated entries where one event happened, the arriving
// account would be told its cost basis is unknown, and the departing one would
// have released shares to nowhere.
//
// A worker that rebuilt inside the per-link loop passes every other test in
// this file.
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

// A RUN'S UNPARSED COUNT IS ITS OWN ACCOUNT'S, not the connection's. The
// rebuild reports one figure for the whole connection, and writing that onto
// every link's run would tell the owner that the account with nothing wrong
// with it has an unreadable operation — the caption that does not match the
// figure beside it.
//
// One link is given an operation of a type this program has never heard of; the
// other is given nothing at all.
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

// -------------------------------------------------------------------------
// how far back a run reads
// -------------------------------------------------------------------------

// EVERY RUN READS THE WHOLE HISTORY, FROM THE DAY THE ACCOUNT WAS OPENED. The
// broker rewrites old operations after the fact, so a run bounded by the last
// successful sync would never see a correction — and worse, SyncMirror marks
// everything it does not find as gone, so the first bounded run would bury the
// entire history before its own window.
//
// The assertion is on the SECOND run's request: after a successful run there is
// a last-successful-sync time to narrow by, and that is exactly the moment the
// mistake would be made.
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

// -------------------------------------------------------------------------
// what stops a run, and what it does about it
// -------------------------------------------------------------------------

// A connection the owner switched off is left completely alone: no broker call,
// no run row, no error. The line is Debug because a disabled connection is a
// routine state, not news.
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
	assertOneRecordAt(t, f.logs, connectionNotActiveMessage, slog.LevelDebug, "disabled")
}

// A TOKEN THE BROKER REFUSES IS NOT A REASON TO RETRY. The connection is parked
// as needing a new token, the run is recorded failed — and Work returns NIL, so
// the queue stops instead of hammering the broker with a credential that will
// never work again.
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

// Any other failure IS worth retrying, so it comes back out of Work and River
// takes it from there. The connection keeps its status: a broker that answered
// 500 has said nothing whatever about the token.
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

// EVERY RUN THIS WORKER STARTED IS CLOSED, including the ones that had already
// succeeded when a later link failed. A run left "running" for good is how the
// store reports a crash, and a failure this worker saw and handled is not one.
func TestSyncWorkerClosesEveryRunItStartedWhenALaterLinkFails(t *testing.T) {
	f := newWorkerFixture(t)
	second := f.secondLink(t)
	// The first link's page is answered; the second one's request is what
	// fails, because the stub answers per rpc and the failure is switched on
	// after the first call has been served.
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

// THE SUMMARY IS THE RECORD OF THE WORK, AND THE WORK HAPPENED. A run that
// carried the whole history across and then could not close its own log entry
// has done everything a sync is for; leaving it with no summary line would make
// the one run that most needs explaining the only one that says nothing at all.
//
// The run's log row is deleted while the run waits at the broker's door — the
// one moment it is certainly open and certainly not being written to — so that
// closing it afterwards fails for a reason the worker cannot dodge.
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

	rec := assertOneRecordAt(t, f.logs, "tinvest: a sync run finished", slog.LevelInfo, f.conn.ID.String())
	// The figures too: a summary that survived but reported nothing would be
	// the same silence in a different shape.
	assertAttrIs(t, rec, "read", "1")
	assertAttrIs(t, rec, "added", "1")
}

// A FAILED RUN PUBLISHES THE UNPARSED COUNT IT TOOK, NOT THE ZERO IT STARTED
// WITH. The figure is filled in by the reconciliation loop, and a run that
// fails before that loop never reaches it — so a worker that simply wrote out
// what it was holding would file "0 unreadable operations" on a run that never
// counted any, in the same column, in the same shape, as a run that counted and
// found none. That is the confusion the reconciliation has a whole "not
// checked" verdict to avoid, and this column has no such state to hide in.
//
// The run below is stopped at the reconciliation — past the mirror and past the
// rebuild, which is what marks a row unreadable in the first place — with one
// operation of a type this program has never heard of already in it.
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
	// The neighbouring figure, for contrast: the reconciliation says "not
	// checked" rather than "everything agrees", which is the very distinction
	// the count above now keeps as well.
	if runs[0].ReconcileStatus != ReconcileNotChecked {
		t.Errorf("reconcile = %q, want %q", runs[0].ReconcileStatus, ReconcileNotChecked)
	}
}

// A trigger the run log's own CHECK constraint would refuse is refused before a
// run is opened, rather than at the INSERT — where it would surface as a
// database error from inside a worker, on the one run that used it.
//
// AND IT IS NOT HANDED TO THE RETRY MACHINE. A queued job's arguments never
// change, so returning the error would buy two dozen further attempts at
// exactly the same word, each one shouting at Error level, spread over River's
// growing backoff — the same waste a refused token was singled out to avoid.
// The line is written once and the job is let go.
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
	// Dropped is not the same as unnoticed: the one line about it has to be
	// visible at the level this application runs at, and has to name the word
	// nothing could read — it is the only record the job leaves anywhere.
	assertOneRecordAt(t, f.logs,
		"tinvest: a sync job names a trigger the run log cannot store, dropping it",
		slog.LevelError, `unknown sync trigger: "whenever"`)
}

// A connection whose owner has not yet chosen which broker accounts to import
// is the ordinary state between "the token was accepted" and "the accounts were
// picked". There is nothing to sync and nothing has gone wrong, so the broker is
// not called at all.
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
	assertOneRecordAt(t, f.logs, noLinksMessage, slog.LevelDebug, f.conn.ID.String())
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
	assertOneRecordAt(t, f.logs, connectionGoneMessage, slog.LevelDebug, f.conn.ID.String())
}

// A PLANNED STOP IS NOT A BREAKAGE, AND NEITHER HALF OF THIS FILE MAY SAY IT
// IS. Shutting the process down cancels the running job's context, and every
// read underneath it fails at once — the dispatcher's listing of connections
// and the sync worker's reading of one alike. At Error level those would be the
// loudest lines of an ordinary restart, and would train the owner to read
// "error" as "ignore".
//
// Both workers are exercised here because the rule lives in a helper each of
// them has to remember to call, and a rule that has to be remembered gets
// forgotten: these two lines are two of the several it had been forgotten at.
func TestACancelledPassIsLoggedAsRoutineAndNotAsAFailure(t *testing.T) {
	f := newWorkerFixture(t)
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()

	dispatchLogs := &logCapture{}
	dispatcher := NewDispatchWorker(f.store, &recordingInserter{}, slog.New(dispatchLogs))
	if err := dispatcher.Work(ctx, &river.Job[SyncDispatchArgs]{JobRow: &rivertype.JobRow{ID: 1}}); err == nil {
		t.Fatal("the dispatcher returned nil though its context was cancelled")
	}
	assertOneRecordAt(t, dispatchLogs,
		"tinvest: list the active connections failed", slog.LevelDebug, "context canceled")

	if err := f.worker.Work(ctx, &river.Job[SyncArgs]{
		JobRow: &rivertype.JobRow{ID: 1},
		Args:   SyncArgs{ConnectionID: f.conn.ID, Trigger: "schedule"},
	}); err == nil {
		t.Fatal("the sync worker returned nil though its context was cancelled")
	}
	assertOneRecordAt(t, f.logs,
		"tinvest: read the connection to sync failed", slog.LevelDebug, "context canceled")
}

// -------------------------------------------------------------------------
// two runs of one connection at once
// -------------------------------------------------------------------------

// TWO SIMULTANEOUS RUNS OF ONE CONNECTION LEAVE ONE MIRROR AND ONE JOURNAL.
//
// This is the property the whole task turns on, and BOTH HALVES OF IT REST ON
// THE MIRROR'S LOCK. SyncMirror's transaction takes a lock on the connection row
// before it reads anything, so the comparison that decides what to insert cannot
// run twice against the same stored state.
//
// The journal's dedup index (operations_dedup_idx over
// account_id/source/external_id, migration 0005) is a second line and not a
// substitute: it catches two runs that agree on WHICH mirror rows exist, because
// then the entries they build carry the same names and the loser's whole delta —
// one transaction — is refused entire. It catches nothing when the two runs each
// inserted the operation into the mirror, because the two rows have different
// ids and so the two journal entries have different names. Measured: with the
// lock removed this test reports two mirror rows AND two journal operations.
//
// So ONE of the two runs may legitimately fail (the loser's delta refused, the
// run recorded failed, River retrying it against a journal that is by then
// already right); what may not happen is a duplicate. Both are asserted.
//
// The two runs are held at the broker's door until both have arrived, so that
// the overlap is observed rather than hoped for.
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

// -------------------------------------------------------------------------
// the dispatcher
// -------------------------------------------------------------------------

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

// The dispatcher queues one job per ACTIVE connection and nothing for the
// others — a connection whose token was refused would otherwise be asked again
// every hour for as long as it existed.
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
	w := NewDispatchWorker(f.store, inserter, slog.New(&logCapture{}))
	if err := w.Work(f.ctx, &river.Job[SyncDispatchArgs]{JobRow: &rivertype.JobRow{ID: 1}}); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if len(inserter.args) != 1 {
		t.Fatalf("%d jobs queued, want 1 (the active connection only): %+v", len(inserter.args), inserter.args)
	}
	if inserter.args[0] != (SyncArgs{ConnectionID: f.conn.ID, Trigger: "schedule"}) {
		t.Errorf("queued %+v, want {%s schedule}", inserter.args[0], f.conn.ID)
	}
	// THE WHOLE OPTIONS VALUE, not one flag of it. ByArgs alone says the job is
	// unique by its arguments and says nothing about ACROSS WHICH STATES, which
	// is where the entire schedule lives: River's default set includes
	// `completed`, and a dispatcher that queued with the default would have
	// every hour after the first skipped as a duplicate of a job long finished.
	// A comparison that stopped at ByArgs would call that correct.
	//
	// AND WRITTEN OUT RATHER THAN TAKEN FROM SyncInsertOpts. Comparing what was
	// queued against the very function that built it proves the dispatcher and
	// the owner's button agree — and proves nothing whatever about what they
	// agree ON, because a change to that function moves both sides together.
	// Measured: with ByState deleted from SyncInsertOpts, a DeepEqual against
	// SyncInsertOpts() stays green and this literal goes red.
	want := &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
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
	logs := &logCapture{}
	inserter := &recordingInserter{}

	w := NewDispatchWorker(f.store, inserter, slog.New(logs))
	if err := w.Work(f.ctx, &river.Job[SyncDispatchArgs]{JobRow: &rivertype.JobRow{ID: 1}}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(inserter.args) != 0 {
		t.Errorf("%d jobs queued, want 0", len(inserter.args))
	}
	rec := assertOneRecordAt(t, logs, nothingToSyncMessage, slog.LevelDebug, "connections")
	assertAttrIs(t, rec, "connections", "0")
}

// A queue that will not take the job is this worker's own failure, so it comes
// back out and River retries the dispatch.
func TestDispatchWorkerReturnsAQueueThatWillNotTakeTheJob(t *testing.T) {
	f := newFixture(t)
	boom := errors.New("queue is down")
	w := NewDispatchWorker(f.store, &recordingInserter{err: boom}, slog.New(&logCapture{}))

	if err := w.Work(f.ctx, &river.Job[SyncDispatchArgs]{JobRow: &rivertype.JobRow{ID: 1}}); !errors.Is(err, boom) {
		t.Fatalf("Work returned %v, want %v", err, boom)
	}
}

// -------------------------------------------------------------------------
// uniqueness, against the real queue
// -------------------------------------------------------------------------

// THE UNIQUENESS COVERS THE CONNECTION AND NOT THE TRIGGER. River hashes the
// encoded args, so a plain ByArgs would put the schedule's job and the owner's
// button in two different unique classes and let them run at once over one
// connection — which is precisely the overlap the uniqueness exists to prevent.
// The `river:"unique"` tag on ConnectionID alone is what narrows it, and this
// test is against the real queue because that tag's effect lives in River, not
// in this package.
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

// THE SCHEDULE HAS TO KEEP WORKING AFTER THE FIRST RUN. Uniqueness is what
// stops two syncs of one connection overlapping; it must not stop the NEXT
// hour's sync of a connection whose previous one is over and done with.
//
// This is not a hypothetical: River's own default set of unique states includes
// `completed`, so a finished job goes on holding its unique key until the job
// cleaner removes the row — hours later — and every dispatch in between is
// skipped. The job is therefore taken through the real queue to completion here
// rather than simulated, because what is being checked is exactly how River
// treats a job it has finished.
func TestASecondSyncIsQueuedOnceTheFirstHasFinished(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx

	worker := &doneWorker{done: make(chan struct{})}
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	client, err := river.NewClient(riverpgxv5.New(f.pool), &river.Config{
		Logger:  slog.New(&logCapture{}),
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

// -------------------------------------------------------------------------
// the shapes the rest of the application depends on
// -------------------------------------------------------------------------

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

// The queue this package declares for itself must stay the real client's own
// method: a signature moving under it is a compile error here rather than a
// wiring failure in cmd/babki.
var _ jobInserter = (*river.Client[pgx.Tx])(nil)

// A token that will not decrypt is a different fault from a token the broker
// refused — the key changed under the ciphertext, most plainly when a database
// backup is restored onto a host whose BABKI_ENCRYPTION_KEY was generated
// afresh. The REMEDY is the same one, though: a token pasted now is sealed with
// the key this process holds. So the connection is parked where a revoked token
// parks, which is the only state the screen offers the owner a new token from.
//
// Before this, the worker returned the error and left the connection "active":
// the queue retried hourly for a key it could not conjure, and every screen
// went on saying the import was fine while it had silently stopped.
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
