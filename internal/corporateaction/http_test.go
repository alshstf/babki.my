package corporateaction_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

// queuedRecheck records what it was asked for.
type queuedRecheck struct {
	calls    int
	accounts []uuid.UUID
	answer   int
}

func (q *queuedRecheck) QueueRecheckForAccounts(_ context.Context, accountIDs []uuid.UUID) (int, error) {
	q.calls++
	q.accounts = append(q.accounts, accountIDs...)
	return q.answer, nil
}

// apiFixture is the registry behind its HTTP door, signed in as the owner. It
// builds its space through /api/v1/setup, since newFixture's user has no usable
// password, and is otherwise shaped like the fixture so its helpers work.
type apiFixture struct {
	fixture
	url     string
	client  *http.Client
	recheck *queuedRecheck
	journal *flakyJournal
	queue   *recordedQueue
}

// flakyJournal fails every write while away is set.
type flakyJournal struct {
	real *operation.Service
	away bool
}

func (j *flakyJournal) BuildAndApplyImportDelta(ctx context.Context, spaceID, accountID uuid.UUID,
	build func(journal []operation.Operation) (operation.ImportDelta, error),
) (operation.ImportDelta, []operation.Operation, []operation.ImportRefusal, error) {
	if j.away {
		return operation.ImportDelta{}, nil, nil, errors.New("the database is away")
	}
	return j.real.BuildAndApplyImportDelta(ctx, spaceID, accountID, build)
}

// recordedQueue is a job queue that keeps what it was handed.
type recordedQueue struct {
	args []river.JobArgs
	opts []*river.InsertOpts
}

func (q *recordedQueue) Insert(_ context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	q.args = append(q.args, args)
	q.opts = append(q.opts, opts)
	return &rivertype.JobInsertResult{}, nil
}

func newAPIFixture(t *testing.T) apiFixture {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()

	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	store := corporateaction.NewStore(pool)
	ops := operation.NewStore(pool)
	svc := operation.NewService(ops)
	recheck := &queuedRecheck{}
	journal, queue := &flakyJournal{real: svc}, &recordedQueue{}
	materializer := corporateaction.NewMaterializer(store, journal, instrument.NewStore(pool), recheck, nil)

	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	corporateaction.NewHandler(store, materializer, queue, auth, sm, slog.Default()).Mount(srv)

	base, client := apitest.Serve(t, srv.Handler())

	var spaceID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM spaces LIMIT 1`).Scan(&spaceID); err != nil {
		t.Fatalf("read the space setup created: %v", err)
	}
	acc, err := account.NewStore(pool).Create(ctx, spaceID, nil, "Брокер", account.TypeBrokerage, "USD", "")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	amazon, err := instrument.NewStore(pool).Create(ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Amazon", Ticker: "AMZN", ISIN: amazonISIN, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("instrument: %v", err)
	}
	return apiFixture{
		fixture: fixture{
			ctx: ctx, pool: pool, store: store, ops: ops, svc: svc,
			materializer: materializer, spaceID: spaceID,
			accountID: acc.ID, amazonID: amazon.ID,
		},
		url: base, client: client, recheck: recheck, journal: journal, queue: queue,
	}
}

// do sends one request as the signed-in owner and returns the status and body.
func (a *apiFixture) do(t *testing.T, method, path, body string) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, a.url+path, rd)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, out
}

// The journal is already split when the request answers.
func TestRecordingASplitReachesTheJournalBeforeItAnswers(t *testing.T) {
	f := newAPIFixture(t)
	f.buy(t, f.accountID, "2021-05-04", "1", -320_000)

	resp, body := f.do(t, http.MethodPost, "/api/v1/instrument-events", `{
		"kind": "split", "isin": "`+amazonISIN+`", "effective_on": "2022-06-06",
		"ratio_from": 1, "ratio_to": 20,
		"source_ref": "https://ir.aboutamazon.com/news-release/2022"
	}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, want 201: %s", resp.StatusCode, body)
	}

	var written struct {
		Event struct {
			ID           string `json:"id"`
			Source       string `json:"source"`
			Materialized bool   `json:"materialized"`
		} `json:"event"`
		RowsAdded       int `json:"rows_added"`
		AccountsTouched int `json:"accounts_touched"`
		RecheckQueued   int `json:"recheck_queued"`
	}
	if err := json.Unmarshal(body, &written); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if written.Event.Source != "manual" {
		t.Errorf("source = %q, want manual — the source is the server's to set, never the request's", written.Event.Source)
	}
	if !written.Event.Materialized {
		t.Errorf("materialized = false on a split, which this program does carry into journals")
	}
	if written.RowsAdded != 1 || written.AccountsTouched != 1 {
		t.Errorf("rows_added = %d, accounts_touched = %d, want 1 and 1",
			written.RowsAdded, written.AccountsTouched)
	}
	// One share bought before the split is twenty after it.
	if held := f.held(t, f.accountID); held.String() != "20" {
		t.Errorf("the account holds %s after the answer came back, want 20", held)
	}
	if f.recheck.calls != 1 {
		t.Errorf("the rechecker was asked %d times, want 1 — a journal changed and a verdict "+
			"about it is now stale", f.recheck.calls)
	}
}

// The evidence link is required: a ratio nobody can check would reach every
// holder's journal.
func TestASplitNoEvidenceBacksIsRefused(t *testing.T) {
	f := newAPIFixture(t)
	resp, body := f.do(t, http.MethodPost, "/api/v1/instrument-events", `{
		"kind": "split", "isin": "`+amazonISIN+`", "effective_on": "2022-06-06",
		"ratio_from": 1, "ratio_to": 20, "source_ref": ""
	}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", resp.StatusCode, body)
	}
	events, err := f.store.List(f.ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("the registry holds %d events after a refused request, want none", len(events))
	}
}

// An exchange row cannot be deleted: the next run would write it back.
func TestTheExchangesOwnRowCannotBeDeleted(t *testing.T) {
	f := newAPIFixture(t)
	e, err := f.store.Create(f.ctx, corporateaction.Event{
		Kind: corporateaction.KindSplit, ISIN: amazonISIN, EffectiveOn: date("2022-06-06"),
		RatioFrom: 1, RatioTo: 20,
		Source: corporateaction.SourceMOEX, SourceRef: "https://iss.moex.com/", MOEXSecID: "AMZN-RM",
	})
	if err != nil {
		t.Fatalf("seed an exchange row: %v", err)
	}

	resp, body := f.do(t, http.MethodDelete, "/api/v1/instrument-events/"+e.ID.String(), "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", resp.StatusCode, body)
	}
	if _, err := f.store.ByID(f.ctx, e.ID); err != nil {
		t.Errorf("the exchange's row is gone after a refused delete: %v", err)
	}
}

// Deleting a hand-recorded event removes its journal rows before the
// answer.
func TestDeletingAHandRecordedEventTakesItsJournalRowsWithIt(t *testing.T) {
	f := newAPIFixture(t)
	f.buy(t, f.accountID, "2021-05-04", "1", -320_000)
	e := f.splitEvent(t, "2022-06-06", 1, 20)
	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if held := f.held(t, f.accountID); held.String() != "20" {
		t.Fatalf("the account holds %s before the delete, want 20", held)
	}

	resp, body := f.do(t, http.MethodDelete, "/api/v1/instrument-events/"+e.ID.String(), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", resp.StatusCode, body)
	}
	var written struct {
		RowsRemoved     int `json:"rows_removed"`
		AccountsTouched int `json:"accounts_touched"`
	}
	if err := json.Unmarshal(body, &written); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if written.RowsRemoved != 1 || written.AccountsTouched != 1 {
		t.Errorf("rows_removed = %d, accounts_touched = %d, want 1 and 1",
			written.RowsRemoved, written.AccountsTouched)
	}
	if held := f.held(t, f.accountID); held.String() != "1" {
		t.Errorf("the account holds %s after the event was removed, want 1", held)
	}
	if len(f.registryRows(t, f.accountID)) != 0 {
		t.Errorf("a registry row outlived the event that asked for it")
	}
}

// A run that changes nothing asks for no recheck.
func TestAMaterializationThatChangesNothingAsksForNoRecheck(t *testing.T) {
	f := newAPIFixture(t)
	// The only purchase is after the split, already in the new quantity.
	f.buy(t, f.accountID, "2023-01-10", "5", -500_000)

	resp, body := f.do(t, http.MethodPost, "/api/v1/instrument-events", `{
		"kind": "split", "isin": "`+amazonISIN+`", "effective_on": "2022-06-06",
		"ratio_from": 1, "ratio_to": 20,
		"source_ref": "https://ir.aboutamazon.com/news-release/2022"
	}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, want 201: %s", resp.StatusCode, body)
	}
	var written struct {
		RowsAdded       int `json:"rows_added"`
		AccountsTouched int `json:"accounts_touched"`
		RecheckQueued   int `json:"recheck_queued"`
	}
	if err := json.Unmarshal(body, &written); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if written.RowsAdded != 0 || written.AccountsTouched != 0 {
		t.Errorf("rows_added = %d, accounts_touched = %d, want 0 and 0 — nobody held the paper on the day",
			written.RowsAdded, written.AccountsTouched)
	}
	if f.recheck.calls != 0 {
		t.Errorf("the rechecker was asked %d times for a run that changed nothing, want 0", f.recheck.calls)
	}
	if held := f.held(t, f.accountID); held.String() != "5" {
		t.Errorf("the account holds %s, want 5 — a purchase after the split is already in the new quantity", held)
	}
}

// A 1:1 split is refused; 1:1 conversions and spin-offs are not (see
// TestAConversionWaitsForThePaperItProducesToBeCatalogued).
func TestASplitOfOneToOneIsRefused(t *testing.T) {
	f := newAPIFixture(t)

	resp, body := f.do(t, http.MethodPost, "/api/v1/instrument-events", `{
		"kind": "split", "isin": "`+amazonISIN+`",
		"effective_on": "2024-02-27", "ratio_from": 1, "ratio_to": 1,
		"source_ref": "https://www.moex.com/n67851"
	}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 — a split of one to one multiplies every holding by one: %s",
			resp.StatusCode, body)
	}
}

// A conversion is recorded even when its produced paper is not catalogued,
// says so in its own field, and writes the pair as soon as the paper is
// catalogued.
func TestAConversionWaitsForThePaperItProducesToBeCatalogued(t *testing.T) {
	f := newAPIFixture(t)
	f.buy(t, f.accountID, "2021-05-04", "4", -320_000)

	const producedISIN = "RU000A107UL4"
	resp, body := f.do(t, http.MethodPost, "/api/v1/instrument-events", `{
		"kind": "conversion", "isin": "`+amazonISIN+`", "result_isin": "`+producedISIN+`",
		"effective_on": "2024-02-27", "ratio_from": 1, "ratio_to": 1,
		"source_ref": "https://www.moex.com/n67851"
	}`)
	// 1:1 is accepted: TCS Group receipts became МКПАО «ТКС Холдинг»
	// shares unit for unit on 2024-02-27.
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, want 201 — one for one is what a conversion usually is: %s", resp.StatusCode, body)
	}

	var written struct {
		Event struct {
			ID               string  `json:"id"`
			Materialized     bool    `json:"materialized"`
			NotCountedReason *string `json:"not_counted_reason"`
		} `json:"event"`
		RowsAdded int `json:"rows_added"`
	}
	if err := json.Unmarshal(body, &written); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	// The kind is materialized; this event is not. Separate questions.
	if !written.Event.Materialized {
		t.Errorf("materialized = false on a conversion, though conversions are carried into journals now")
	}
	if written.Event.NotCountedReason == nil || *written.Event.NotCountedReason != "result_not_in_catalog" {
		t.Errorf("not_counted_reason = %v, want result_not_in_catalog — the paper it produces has no catalog row",
			written.Event.NotCountedReason)
	}
	if written.RowsAdded != 0 {
		t.Errorf("rows_added = %d, want 0 — there is no paper to point the arriving leg at", written.RowsAdded)
	}
	if held := f.held(t, f.accountID); held.String() != "4" {
		t.Errorf("the account holds %s, want 4 — nothing was written", held)
	}
	if f.recheck.calls != 0 {
		t.Errorf("the rechecker was asked %d times though no journal changed, want 0", f.recheck.calls)
	}

	// Catalogue the produced paper and run again; the event is unchanged.
	if _, err := instrument.NewStore(f.pool).Create(f.ctx, instrument.Instrument{
		Type: instrument.TypeShare, Name: "Т-Технологии", Ticker: "T", ISIN: producedISIN, Currency: "USD",
	}); err != nil {
		t.Fatalf("catalogue the produced paper: %v", err)
	}
	if _, err := f.materializer.ForISIN(f.ctx, amazonISIN); err != nil {
		t.Fatalf("materialize after cataloguing: %v", err)
	}

	resp, body = f.do(t, http.MethodGet, "/api/v1/instrument-events", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", resp.StatusCode, body)
	}
	var listed struct {
		Events []struct {
			ID               string  `json:"id"`
			NotCountedReason *string `json:"not_counted_reason"`
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if len(listed.Events) != 1 {
		t.Fatalf("the registry lists %d events, want 1", len(listed.Events))
	}
	if listed.Events[0].NotCountedReason != nil {
		t.Errorf("not_counted_reason = %v after the paper was catalogued, want null",
			*listed.Events[0].NotCountedReason)
	}
	// Two units left and one arrived: the ratio is two for one.
	if held := f.held(t, f.accountID); held.String() != "0" {
		t.Errorf("the account holds %s of the old paper, want 0 — a conversion takes the whole holding", held)
	}
}

// TestTheRegistryListsWhatItHolds, newest effective date first.
func TestTheRegistryListsWhatItHolds(t *testing.T) {
	f := newAPIFixture(t)
	f.splitEvent(t, "2022-06-06", 1, 20)
	f.splitEvent(t, "2024-06-10", 1, 10)

	resp, body := f.do(t, http.MethodGet, "/api/v1/instrument-events", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", resp.StatusCode, body)
	}
	var listed struct {
		Events []struct {
			EffectiveOn string `json:"effective_on"`
			SourceRef   string `json:"source_ref"`
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if len(listed.Events) != 2 {
		t.Fatalf("listed %d events, want 2", len(listed.Events))
	}
	if listed.Events[0].EffectiveOn != "2024-06-10" {
		t.Errorf("first listed event is %s, want the newest (2024-06-10)", listed.Events[0].EffectiveOn)
	}
	if listed.Events[0].SourceRef == "" {
		t.Errorf("the evidence link is not published, so the screen cannot show what the row rests on")
	}
}

// An ISIN typed in lower case is stored upper-cased, or it would match
// no holder (#202).
func TestAnEventsISINIsStoredInOneSpelling(t *testing.T) {
	f := newAPIFixture(t)
	f.buy(t, f.accountID, "2021-05-04", "1", -323_000)

	resp, body := f.do(t, "POST", "/api/v1/instrument-events",
		`{"kind":"split","isin":" us0231351067 ","effective_on":"2022-06-06","ratio_from":1,"ratio_to":20,`+
			`"source_ref":"https://ir.aboutamazon.com/"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d: %s", resp.StatusCode, body)
	}
	if got, want := f.held(t, f.accountID), decimal.RequireFromString("20"); !got.Equal(want) {
		t.Errorf("holding = %s, want %s — the event must reach the paper it names", got, want)
	}
}

// A malformed ISIN matches nothing, and a date before the journal's floor
// makes rows the journal refuses.
func TestAnEventIsRefusedForWhatWouldMakeItSilentlyUseless(t *testing.T) {
	f := newAPIFixture(t)
	for name, body := range map[string]string{
		"an isin that is not one": `{"kind":"split","isin":"AMZN","effective_on":"2022-06-06","ratio_from":1,"ratio_to":20,"source_ref":"x"}`,
		"a result isin that is not one": `{"kind":"conversion","isin":"US0231351067","result_isin":"T","effective_on":"2022-06-06",` +
			`"ratio_from":1,"ratio_to":1,"source_ref":"x"}`,
		"a date before the journal's floor": `{"kind":"split","isin":"US0231351067","effective_on":"1800-06-06","ratio_from":1,"ratio_to":20,"source_ref":"x"}`,
	} {
		if resp, out := f.do(t, "POST", "/api/v1/instrument-events", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400: %s", name, resp.StatusCode, out)
		}
	}
}
