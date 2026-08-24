package corporateaction_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

// queuedRecheck is a rechecker that records what it was asked for. A real one
// needs a job queue and a broker link; what every test here is about is whether
// the ask happens at all and with which accounts.
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

// apiFixture is the registry behind its own HTTP door, signed in as the owner.
//
// It builds its own space through /api/v1/setup rather than reusing
// newFixture's, because that one seeds a user with a hash no password matches —
// perfectly good for the store-level tests and useless for a session. The rest
// of the fixture is assembled to be the same shape, so every helper on it
// (buy, held, splitEvent, registryRows) works here unchanged.
type apiFixture struct {
	fixture
	url     string
	client  *http.Client
	recheck *queuedRecheck
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
	materializer := corporateaction.NewMaterializer(store, ops, svc, instrument.NewStore(pool), recheck, nil)

	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	corporateaction.NewHandler(store, materializer, auth, sm, slog.Default()).Mount(srv)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err := client.Post(ts.URL+"/api/v1/setup", "application/json",
		strings.NewReader(`{"space_name":"S","username":"alex","display_name":"A","password":"secret123"}`))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup: %v %v", err, resp)
	}
	_ = resp.Body.Close()

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
		url: ts.URL, client: client, recheck: recheck,
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

// TestRecordingASplitReachesTheJournalBeforeItAnswers is the whole point of
// materializing inside the request: the owner records Amazon's 20:1 and the
// position is already right when the answer comes back, rather than an hour
// later when the sweep runs.
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
	// The figure the answer describes, read back from the journal itself: one
	// share bought before the split is twenty after it.
	if held := f.held(t, f.accountID); held.String() != "20" {
		t.Errorf("the account holds %s after the answer came back, want 20", held)
	}
	if f.recheck.calls != 1 {
		t.Errorf("the rechecker was asked %d times, want 1 — a journal changed and a verdict "+
			"about it is now stale", f.recheck.calls)
	}
}

// TestASplitNoEvidenceBacksIsRefused: a ratio nobody can check would be carried
// into every holder's journal on one person's word, so the link is the one field
// of this request that is not optional.
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

// TestTheExchangesOwnRowCannotBeDeleted: the job that wrote it reads the
// exchange's table on every run and would write it back, so a deletion would
// last until the next run and no longer. Refused by name rather than accepted
// and quietly undone.
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

// TestDeletingAHandRecordedEventTakesItsJournalRowsWithIt: the same
// materialization runs, finds the registry no longer asks for those rows, and
// removes them before the answer — so the position is back to what it was.
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

// TestAMaterializationThatChangesNothingAsksForNoRecheck. A sweep walks every
// holder of every paper and almost always writes nothing; asking for a fresh
// broker comparison each time would turn a no-op into a broker read per
// connection, daily, for ever.
func TestAMaterializationThatChangesNothingAsksForNoRecheck(t *testing.T) {
	f := newAPIFixture(t)
	// Nobody held Amazon on the effective day: the only purchase is after it,
	// and a purchase made after a split is already in the new quantity.
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

// TestASplitOfOneToOneIsRefused keeps the emptiness rule where it is true.
//
// A split has nothing to say but its ratio, so one for one multiplies every
// holding by one: a row claiming an event, carried into the journal of every
// account holding the paper, saying nothing happened. The other two kinds are
// not like that at all — see TestAConversionWaitsForThePaperItProducesToBeCatalogued,
// where one for one is the owner's own case — and this test exists so that the
// narrowing cannot be widened back by accident.
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

// TestAConversionWaitsForThePaperItProducesToBeCatalogued. The facts are
// perishable — a fund converted in 2023 and nobody can go back and ask the
// registrar again — so a conversion is recorded whether or not anything here can
// yet act on it. What it cannot do is point a journal row at a paper the catalog
// has no row for, and THAT is the only thing standing between the record and the
// journal now that both legs of a conversion exist. So the row says which of the
// two it is, in a field of its own, and the moment the paper is catalogued the
// very same event writes the pair.
func TestAConversionWaitsForThePaperItProducesToBeCatalogued(t *testing.T) {
	f := newAPIFixture(t)
	f.buy(t, f.accountID, "2021-05-04", "4", -320_000)

	const producedISIN = "RU000A107UL4"
	resp, body := f.do(t, http.MethodPost, "/api/v1/instrument-events", `{
		"kind": "conversion", "isin": "`+amazonISIN+`", "result_isin": "`+producedISIN+`",
		"effective_on": "2024-02-27", "ratio_from": 1, "ratio_to": 1,
		"source_ref": "https://www.moex.com/n67851"
	}`)
	// ONE FOR ONE IS ACCEPTED HERE, and the exact request above is the reason:
	// it is the owner's own case — TCS Group receipts became shares of МКПАО
	// «ТКС Холдинг» unit for unit on 2024-02-27 — and it is what a conversion
	// most often looks like, since a redomiciliation or an ISIN change moves
	// the identity and leaves the count alone. Equal sides are empty only for a
	// SPLIT, which has nothing but the ratio to say; that refusal has a test of
	// its own (see TestASplitOfOneToOneIsRefused).
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
	// The KIND is carried into journals; THIS event is not, and the two answers
	// are separate fields because they are separate questions.
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

	// Now catalogue the paper the conversion produces and let the registry run
	// again. NOTHING ABOUT THE EVENT CHANGES — it is the same row, recorded
	// before the paper existed here — and that is the point: the fact was always
	// true and only this program's ability to express it was missing.
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
