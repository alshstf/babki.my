package tinvest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/secretbox"
	"babki.my/babki/internal/platform/testdb"
)

// demoToken is the token every test here pastes. It is a value with no digits
// in common with anything else in these tests, so a search for it in a response
// body cannot match by accident — see TestNoResponseEverCarriesTheToken, which
// is the reason it is a constant rather than "t".
const demoToken = "t.zZq7Xv91LkPmWn4TokenSecretValue"

// brokerAccountsBody is what the fake gateway answers GetAccounts with: an
// ordinary brokerage account, an ИИС, an «инвесткопилка» this program does not
// import, and a CLOSED brokerage account, which it does — the contract says the
// list is filtered by kind and not by status, and the fourth row is what makes
// that claim checkable rather than merely written down.
const brokerAccountsBody = `{"accounts":[
	{"id":"2000000001","type":"ACCOUNT_TYPE_TINKOFF","name":"Брокерский счёт",
	 "status":"ACCOUNT_STATUS_OPEN","openedDate":"2019-03-14T00:00:00Z"},
	{"id":"2000000002","type":"ACCOUNT_TYPE_TINKOFF_IIS","name":"ИИС",
	 "status":"ACCOUNT_STATUS_OPEN","openedDate":"2021-01-11T00:00:00Z"},
	{"id":"2000000003","type":"ACCOUNT_TYPE_INVEST_BOX","name":"Инвесткопилка",
	 "status":"ACCOUNT_STATUS_OPEN"},
	{"id":"2000000004","type":"ACCOUNT_TYPE_TINKOFF","name":"Закрытый брокерский",
	 "status":"ACCOUNT_STATUS_CLOSED","openedDate":"2017-06-01T00:00:00Z"}
]}`

// fakeBroker stands in for the T-Invest gateway. status 0 means "answer
// normally"; anything else is the status it answers with instead, which is how
// a refused token (401) and an unwell gateway (500) are told apart.
type fakeBroker struct {
	mu     sync.Mutex
	status int
	body   string
	tokens []string
}

func (b *fakeBroker) set(status int, body string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status, b.body = status, body
}

func (b *fakeBroker) seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.tokens...)
}

func (b *fakeBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	status, body := b.status, b.body
	b.tokens = append(b.tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if status != 0 {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
		return
	}
	_, _ = io.WriteString(w, brokerAccountsBody)
}

// fakeInserter records what the request path queued, without a River client or
// a queue behind it. dup makes the next insert report the answer River gives
// when a sync for this connection is already queued.
type fakeInserter struct {
	mu   sync.Mutex
	args []SyncArgs
	opts []*river.InsertOpts
	dup  bool
}

func (f *fakeInserter) Insert(_ context.Context, args river.JobArgs, opts *river.InsertOpts) (
	*rivertype.JobInsertResult, error,
) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sa, ok := args.(SyncArgs); ok {
		f.args = append(f.args, sa)
	}
	f.opts = append(f.opts, opts)
	return &rivertype.JobInsertResult{UniqueSkippedAsDuplicate: f.dup}, nil
}

func (f *fakeInserter) queued() []SyncArgs {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SyncArgs(nil), f.args...)
}

// testAPI is the whole stack under test: the real HTTP server, a real database,
// three signed-in members of one space, and doubles for the two things this
// package must not reach in a test — the broker and the job queue.
type testAPI struct {
	url      string
	owner    *http.Client
	editor   *http.Client
	viewer   *http.Client
	store    *Store
	accounts *account.Store
	broker   *fakeBroker
	inserter *fakeInserter
	spaceID  uuid.UUID
}

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()

	famStore := family.NewStore(pool)
	famSvc := family.NewService(famStore)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)

	_, ownerP, err := famSvc.Setup(ctx, family.SetupParams{
		SpaceName: "S", Username: "owner", DisplayName: "O", Password: "secret123",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	for _, m := range []struct {
		user string
		role family.Role
	}{{"editor", family.RoleEditor}, {"viewer", family.RoleViewer}} {
		if _, err := famSvc.CreateMember(ctx, ownerP, m.user, m.user, "secret123", m.role); err != nil {
			t.Fatalf("create %s: %v", m.user, err)
		}
	}

	broker := &fakeBroker{}
	brokerSrv := httptest.NewServer(broker)
	t.Cleanup(brokerSrv.Close)

	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatalf("secretbox: %v", err)
	}
	store := NewStore(pool)
	accStore := account.NewStore(pool)
	inserter := &fakeInserter{}
	svc := NewService(store, accStore, box,
		func(token string) (*Client, error) {
			return NewClient(brokerSrv.Client(), brokerSrv.URL, token, slog.Default()), nil
		}, inserter, slog.Default())

	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(famSvc, famStore, auth, sm).Mount(srv)
	NewHandler(svc, auth, sm).Mount(srv)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	api := &testAPI{
		url: ts.URL, store: store, accounts: accStore,
		broker: broker, inserter: inserter, spaceID: ownerP.SpaceID,
	}
	api.owner = login(t, ts.URL, "owner")
	api.editor = login(t, ts.URL, "editor")
	api.viewer = login(t, ts.URL, "viewer")
	return api
}

func login(t *testing.T, url, username string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.Post(url+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"username":"`+username+`","password":"secret123"}`))
	if err != nil {
		t.Fatalf("login %s: %v", username, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login %s: status %d", username, resp.StatusCode)
	}
	return c
}

func do(t *testing.T, c *http.Client, method, url, body string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, url, err)
	}
	return resp.StatusCode, string(raw)
}

// createConnection connects the fake broker's first account and returns the
// created connection's id and the raw body of the 201.
func (a *testAPI) createConnection(t *testing.T) (uuid.UUID, string) {
	t.Helper()
	code, body := do(t, a.owner, "POST", a.url+"/api/v1/tinvest/connections",
		`{"token":"`+demoToken+`","accounts":[{"broker_account_id":"2000000001","account_name":"Брокерский Т-Банк"}]}`)
	if code != http.StatusCreated {
		t.Fatalf("create connection: status %d, body %s", code, body)
	}
	var out struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("create connection: decode %s: %v", body, err)
	}
	return out.ID, body
}

// -------------------------------------------------------------------------
// the owner-only rule, on every single endpoint
// -------------------------------------------------------------------------

// endpoint is one route of this module, spelled with a connection id already
// filled in. Every one of them is listed by hand rather than discovered from the
// router, so that a route added tomorrow without a line here is a route this
// test never looked at — and the count below is what says so out loud.
type endpoint struct {
	method, path, body string
}

func allEndpoints(id uuid.UUID) []endpoint {
	conn := "/api/v1/tinvest/connections/" + id.String()
	return []endpoint{
		{"POST", "/api/v1/tinvest/token-check", `{"token":"` + demoToken + `"}`},
		{"GET", "/api/v1/tinvest/connections", ""},
		{
			"POST", "/api/v1/tinvest/connections",
			`{"token":"` + demoToken + `","accounts":[{"broker_account_id":"2000000002","account_name":"ИИС"}]}`,
		},
		{"GET", conn, ""},
		{"PATCH", conn, `{"status":"disabled"}`},
		{"DELETE", conn, ""},
		{"POST", conn + "/sync", ""},
		{"GET", conn + "/runs", ""},
		{"GET", conn + "/unparsed", ""},
	}
}

func TestEveryEndpointRefusesAnEditorAndAViewer(t *testing.T) {
	api := newTestAPI(t)
	id, _ := api.createConnection(t)

	endpoints := allEndpoints(id)
	// Nine routes are mounted (see Handler.Mount) and nine are listed above.
	// Stated as a literal so that mounting a tenth without listing it here fails
	// rather than passing quietly on the nine it does cover.
	if len(endpoints) != 9 {
		t.Fatalf("this test covers %d endpoints, but the module mounts 9", len(endpoints))
	}

	for _, who := range []struct {
		name string
		c    *http.Client
	}{{"editor", api.editor}, {"viewer", api.viewer}} {
		for _, e := range endpoints {
			code, body := do(t, who.c, e.method, api.url+e.path, e.body)
			if code != http.StatusForbidden {
				t.Errorf("%s %s as %s: status %d (body %s), want 403",
					e.method, e.path, who.name, code, body)
			}
		}
	}

	// And the connection is still exactly as the owner left it: a refusal that
	// wrote something first would be a 403 over a completed action.
	conn, err := api.store.ConnectionByID(context.Background(), api.spaceID, id)
	if err != nil {
		t.Fatalf("the connection is gone after the refusals: %v", err)
	}
	if conn.Status != StatusActive {
		t.Errorf("status = %q after the refusals, want %q", conn.Status, StatusActive)
	}
}

// -------------------------------------------------------------------------
// the token never leaves
// -------------------------------------------------------------------------

func TestNoResponseEverCarriesTheToken(t *testing.T) {
	api := newTestAPI(t)
	id, createBody := api.createConnection(t)
	api.seedRunsAndUnparsed(t, id, 1, 1)

	conn := "/api/v1/tinvest/connections/" + id.String()
	bodies := map[string]string{"POST /connections": createBody}
	for _, e := range []endpoint{
		{"POST", "/api/v1/tinvest/token-check", `{"token":"` + demoToken + `"}`},
		{"GET", "/api/v1/tinvest/connections", ""},
		{"GET", conn, ""},
		{"PATCH", conn, `{"token":"` + demoToken + `"}`},
		{"POST", conn + "/sync", ""},
		{"GET", conn + "/runs", ""},
		{"GET", conn + "/unparsed", ""},
		{"DELETE", conn, ""},
	} {
		_, body := do(t, api.owner, e.method, api.url+e.path, e.body)
		bodies[e.method+" "+e.path] = body
	}

	for where, body := range bodies {
		if strings.Contains(body, demoToken) {
			t.Errorf("%s answered with the token itself in the body: %s", where, body)
		}
	}
	// The tail IS published, and this half of the test is what keeps the half
	// above from passing on a server that publishes nothing at all.
	if !strings.Contains(bodies["POST /connections"], `"token_last4":"alue"`) {
		t.Errorf("the created connection does not carry the token's last four characters: %s",
			bodies["POST /connections"])
	}
}

func TestTheTokenTailIsTheLastFourCharacters(t *testing.T) {
	for _, c := range []struct {
		token, want string
	}{
		{"t.zZq7Xv91LkPmWn4TokenSecretValue", "alue"},
		{"abcd", "abcd"},
		{"xy", "xy"},
		{"", ""},
		// Runes, not bytes: cutting the last four BYTES off this would leave
		// half a character behind and put invalid UTF-8 into a JSON response.
		{"токен", "окен"},
	} {
		if got := tokenLast4(c.token); got != c.want {
			t.Errorf("tokenLast4(%q) = %q, want %q", c.token, got, c.want)
		}
	}
}

// -------------------------------------------------------------------------
// creating a connection
// -------------------------------------------------------------------------

func TestCreatingAConnectionMakesRubleBrokerageAccountsAndQueuesTheFirstSync(t *testing.T) {
	api := newTestAPI(t)
	ctx := context.Background()

	code, body := do(t, api.owner, "POST", api.url+"/api/v1/tinvest/connections",
		`{"token":"`+demoToken+`","accounts":[`+
			`{"broker_account_id":"2000000001","account_name":"Т-Банк брокерский"},`+
			`{"broker_account_id":"2000000002","account_name":"Т-Банк ИИС"}]}`)
	if code != http.StatusCreated {
		t.Fatalf("status %d, body %s", code, body)
	}

	accounts, err := api.accounts.ListWithBalance(ctx, api.spaceID)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(accounts))
	}
	for _, a := range accounts {
		// RUB is not decoration: the reconciliation refuses to write the
		// broker's balance mark onto an account kept in anything else (see
		// ErrAccountNotInRubles).
		if a.Currency != "RUB" {
			t.Errorf("account %q currency = %q, want RUB", a.Name, a.Currency)
		}
		if a.Type != account.TypeBrokerage {
			t.Errorf("account %q type = %q, want brokerage", a.Name, a.Type)
		}
		if a.Institution != "Т-Банк" {
			t.Errorf("account %q institution = %q, want Т-Банк", a.Name, a.Institution)
		}
	}

	var out struct {
		ID       uuid.UUID `json:"id"`
		Status   string    `json:"status"`
		Accounts []struct {
			AccountID         uuid.UUID `json:"account_id"`
			BrokerAccountID   string    `json:"broker_account_id"`
			BrokerAccountName string    `json:"broker_account_name"`
			BrokerAccountType string    `json:"broker_account_type"`
			OpenedOn          *string   `json:"opened_on"`
		} `json:"accounts"`
		LastSuccessfulSyncAt *string   `json:"last_successful_sync_at"`
		LastReconcile        *struct{} `json:"last_reconcile"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if out.Status != "active" {
		t.Errorf("status = %q, want active", out.Status)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("linked accounts = %d, want 2", len(out.Accounts))
	}
	if out.Accounts[0].BrokerAccountName != "Брокерский счёт" ||
		out.Accounts[0].BrokerAccountType != "ACCOUNT_TYPE_TINKOFF" {
		t.Errorf("first link kept %q/%q, want the broker's own naming",
			out.Accounts[0].BrokerAccountName, out.Accounts[0].BrokerAccountType)
	}
	if out.Accounts[0].OpenedOn == nil || *out.Accounts[0].OpenedOn != "2019-03-14" {
		t.Errorf("first link opened_on = %v, want 2019-03-14 from the broker", out.Accounts[0].OpenedOn)
	}
	if out.LastSuccessfulSyncAt != nil || out.LastReconcile != nil {
		t.Errorf("a brand new connection claims a sync (%v) or a check (%v); it has had neither",
			out.LastSuccessfulSyncAt, out.LastReconcile)
	}

	queued := api.inserter.queued()
	if len(queued) != 1 {
		t.Fatalf("queued %d syncs, want exactly 1 for the whole connection", len(queued))
	}
	if queued[0].ConnectionID != out.ID || queued[0].Trigger != string(TriggerInitial) {
		t.Errorf("queued %+v, want connection %s with trigger %q", queued[0], out.ID, TriggerInitial)
	}
	// The insert options are the shared ones, not options built here: the
	// button, the schedule and this first import must fall in one class of
	// uniqueness or two of them could run over a connection at once.
	if len(api.inserter.opts) != 1 || api.inserter.opts[0] == nil ||
		!api.inserter.opts[0].UniqueOpts.ByArgs {
		t.Errorf("the first sync was queued with %+v, want SyncInsertOpts()", api.inserter.opts)
	}
}

func TestCreatingAConnectionRefusesWhatItCannotImport(t *testing.T) {
	api := newTestAPI(t)
	base := api.url + "/api/v1/tinvest/connections"
	for _, c := range []struct {
		name, body string
		want       int
	}{
		{
			"an account the token cannot see",
			`{"token":"` + demoToken + `","accounts":[{"broker_account_id":"9999","account_name":"X"}]}`,
			http.StatusBadRequest,
		},
		{
			"an account of a kind this program does not import",
			`{"token":"` + demoToken + `","accounts":[{"broker_account_id":"2000000003","account_name":"X"}]}`,
			http.StatusBadRequest,
		},
		{
			"the same broker account twice",
			`{"token":"` + demoToken + `","accounts":[` +
				`{"broker_account_id":"2000000001","account_name":"A"},` +
				`{"broker_account_id":"2000000001","account_name":"B"}]}`,
			http.StatusBadRequest,
		},
		{
			"no accounts at all",
			`{"token":"` + demoToken + `","accounts":[]}`, http.StatusBadRequest,
		},
		{
			"an empty account name",
			`{"token":"` + demoToken + `","accounts":[{"broker_account_id":"2000000001","account_name":""}]}`,
			http.StatusBadRequest,
		},
		{
			"an empty token",
			`{"token":"","accounts":[{"broker_account_id":"2000000001","account_name":"X"}]}`,
			http.StatusBadRequest,
		},
	} {
		code, body := do(t, api.owner, "POST", base, c.body)
		if code != c.want {
			t.Errorf("%s: status %d (body %s), want %d", c.name, code, body, c.want)
		}
	}

	// Nothing was written by any of them: a refusal that had already created an
	// account would leave the accounts screen holding a stranger.
	accounts, err := api.accounts.ListWithBalance(context.Background(), api.spaceID)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(accounts) != 0 {
		t.Errorf("accounts = %d after six refusals, want 0", len(accounts))
	}
	if got := api.inserter.queued(); len(got) != 0 {
		t.Errorf("queued %d syncs after six refusals, want 0", len(got))
	}
}

func TestABrokerAccountCannotBeImportedTwice(t *testing.T) {
	api := newTestAPI(t)
	api.createConnection(t)

	code, body := do(t, api.owner, "POST", api.url+"/api/v1/tinvest/connections",
		`{"token":"`+demoToken+`","accounts":[{"broker_account_id":"2000000001","account_name":"ещё раз"}]}`)
	if code != http.StatusConflict {
		t.Fatalf("status %d (body %s), want 409: importing one broker account twice "+
			"would put its operations into two accounts", code, body)
	}
	accounts, err := api.accounts.ListWithBalance(context.Background(), api.spaceID)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Errorf("accounts = %d, want 1: the refused request created one anyway", len(accounts))
	}
}

// -------------------------------------------------------------------------
// the broker's two different failures
// -------------------------------------------------------------------------

func TestARefusedTokenAndAnUnreachableBrokerAreDifferentAnswers(t *testing.T) {
	api := newTestAPI(t)
	check := api.url + "/api/v1/tinvest/token-check"

	api.broker.set(http.StatusUnauthorized, `{"code":16,"message":"unauthenticated","description":"40003"}`)
	if code, body := do(t, api.owner, "POST", check, `{"token":"`+demoToken+`"}`); code != http.StatusBadRequest {
		t.Errorf("a refused token: status %d (body %s), want 400", code, body)
	}

	api.broker.set(http.StatusInternalServerError, `{"code":13,"message":"internal"}`)
	if code, body := do(t, api.owner, "POST", check, `{"token":"`+demoToken+`"}`); code != http.StatusBadGateway {
		t.Errorf("an unwell gateway: status %d (body %s), want 502 — a client that "+
			"could not tell this from a refused token would tell the owner to find a new token", code, body)
	}
}

func TestTokenCheckOffersOnlyTheAccountsThisProgramImports(t *testing.T) {
	api := newTestAPI(t)
	code, body := do(t, api.owner, "POST", api.url+"/api/v1/tinvest/token-check",
		`{"token":"`+demoToken+`"}`)
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	var out struct {
		Accounts []struct {
			BrokerAccountID string  `json:"broker_account_id"`
			Name            string  `json:"name"`
			Type            string  `json:"type"`
			OpenedOn        *string `json:"opened_on"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if len(out.Accounts) != 3 {
		t.Fatalf("accounts = %d, want 3: the «инвесткопилка» is not importable and everything "+
			"else the token sees is, closed accounts included", len(out.Accounts))
	}
	if out.Accounts[0].BrokerAccountID != "2000000001" || out.Accounts[1].BrokerAccountID != "2000000002" {
		t.Errorf("accounts = %+v, want the brokerage account and the ИИС first", out.Accounts)
	}
	// The closed one is on the list, and that is the contract's claim rather
	// than an accident: a closed account's history is as real as an open one's,
	// and its settled results are the part worth importing.
	if out.Accounts[2].BrokerAccountID != "2000000004" {
		t.Errorf("accounts = %+v, want the closed brokerage account listed too", out.Accounts)
	}
	if out.Accounts[1].OpenedOn == nil || *out.Accounts[1].OpenedOn != "2021-01-11" {
		t.Errorf("the ИИС opened_on = %v, want 2021-01-11", out.Accounts[1].OpenedOn)
	}
	// Nothing was stored: a token check is a question, not a connection.
	conns, err := api.store.ListConnections(context.Background(), api.spaceID)
	if err != nil {
		t.Fatalf("list connections: %v", err)
	}
	if len(conns) != 0 {
		t.Errorf("connections = %d after a token check, want 0", len(conns))
	}
	if seen := api.broker.seen(); len(seen) != 1 || seen[0] != demoToken {
		t.Errorf("the broker was asked with %v, want exactly the token that was pasted", seen)
	}
}

// -------------------------------------------------------------------------
// updating and deleting
// -------------------------------------------------------------------------

func TestANewTokenTheBrokerAcceptsBringsARevokedConnectionBack(t *testing.T) {
	api := newTestAPI(t)
	ctx := context.Background()
	id, _ := api.createConnection(t)
	if err := api.store.UpdateConnectionStatus(ctx, id, StatusTokenRevoked); err != nil {
		t.Fatalf("park the connection: %v", err)
	}

	const replacement = "t.NewlyPastedReplacementToken9911"
	code, body := do(t, api.owner, "PATCH", api.url+"/api/v1/tinvest/connections/"+id.String(),
		`{"token":"`+replacement+`"}`)
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	if !strings.Contains(body, `"status":"active"`) {
		t.Errorf("status after a token the broker accepted: %s, want active", body)
	}
	if !strings.Contains(body, `"token_last4":"9911"`) {
		t.Errorf("the tail was not replaced: %s", body)
	}
	conn, err := api.store.ConnectionByID(ctx, api.spaceID, id)
	if err != nil {
		t.Fatalf("read connection: %v", err)
	}
	if conn.Status != StatusActive {
		t.Errorf("stored status = %q, want active", conn.Status)
	}
	if string(conn.TokenCiphertext) == replacement {
		t.Errorf("the token is stored in the clear")
	}
}

func TestAStatusSentWithATokenWinsOverTheTokenAcceptance(t *testing.T) {
	api := newTestAPI(t)
	ctx := context.Background()
	id, _ := api.createConnection(t)
	if err := api.store.UpdateConnectionStatus(ctx, id, StatusTokenRevoked); err != nil {
		t.Fatalf("park the connection: %v", err)
	}

	code, body := do(t, api.owner, "PATCH", api.url+"/api/v1/tinvest/connections/"+id.String(),
		`{"token":"t.AnotherPerfectlyGoodToken4242","status":"disabled"}`)
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	if !strings.Contains(body, `"status":"disabled"`) {
		t.Errorf("status = %s, want disabled: a token the broker accepts switches a connection "+
			"back on only when the request says nothing about the status", body)
	}
	if !strings.Contains(body, `"token_last4":"4242"`) {
		t.Errorf("the token was not replaced: %s", body)
	}
}

func TestATokenTheBrokerRefusesReplacesNothing(t *testing.T) {
	api := newTestAPI(t)
	ctx := context.Background()
	id, _ := api.createConnection(t)
	before, err := api.store.ConnectionByID(ctx, api.spaceID, id)
	if err != nil {
		t.Fatalf("read connection: %v", err)
	}

	api.broker.set(http.StatusUnauthorized, `{"code":16,"message":"unauthenticated","description":"40003"}`)
	code, body := do(t, api.owner, "PATCH", api.url+"/api/v1/tinvest/connections/"+id.String(),
		`{"token":"t.SomethingTheBrokerDoesNotLike"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status %d (body %s), want 400", code, body)
	}
	after, err := api.store.ConnectionByID(ctx, api.spaceID, id)
	if err != nil {
		t.Fatalf("read connection: %v", err)
	}
	if after.TokenLast4 != before.TokenLast4 {
		t.Errorf("token_last4 = %q after a refused token, want the old %q",
			after.TokenLast4, before.TokenLast4)
	}
}

func TestUpdatingAConnectionRefusesWhatTheServerAloneMaySay(t *testing.T) {
	api := newTestAPI(t)
	id, _ := api.createConnection(t)
	conn := api.url + "/api/v1/tinvest/connections/" + id.String()
	for _, c := range []struct {
		name, body string
	}{
		{"an empty body", `{}`},
		{"token_revoked, which is the server's own verdict", `{"status":"token_revoked"}`},
		{"a status nobody defines", `{"status":"paused"}`},
		{"an empty token", `{"token":""}`},
	} {
		if code, body := do(t, api.owner, "PATCH", conn, c.body); code != http.StatusBadRequest {
			t.Errorf("%s: status %d (body %s), want 400", c.name, code, body)
		}
	}
}

func TestSwitchingAConnectionOffAsksTheBrokerNothing(t *testing.T) {
	api := newTestAPI(t)
	id, _ := api.createConnection(t)
	before := len(api.broker.seen())

	code, body := do(t, api.owner, "PATCH", api.url+"/api/v1/tinvest/connections/"+id.String(),
		`{"status":"disabled"}`)
	if code != http.StatusOK || !strings.Contains(body, `"status":"disabled"`) {
		t.Fatalf("status %d, body %s", code, body)
	}
	if got := len(api.broker.seen()) - before; got != 0 {
		t.Errorf("the broker was called %d times to switch a connection off, want 0", got)
	}
}

func TestDeletingAConnectionLeavesTheAccountsAndTheirOperations(t *testing.T) {
	api := newTestAPI(t)
	ctx := context.Background()
	id, _ := api.createConnection(t)
	before, err := api.accounts.ListWithBalance(ctx, api.spaceID)
	if err != nil || len(before) != 1 {
		t.Fatalf("accounts before = %d, %v; want 1", len(before), err)
	}

	if code, body := do(t, api.owner, "DELETE", api.url+"/api/v1/tinvest/connections/"+id.String(), ""); code != http.StatusNoContent {
		t.Fatalf("status %d, body %s", code, body)
	}

	after, err := api.accounts.ListWithBalance(ctx, api.spaceID)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(after) != 1 || after[0].ID != before[0].ID {
		t.Errorf("accounts after the disconnect = %+v, want the same one: an account is the "+
			"owner's data and withdrawing a token does not take it away", after)
	}
	conns, err := api.store.ListConnections(ctx, api.spaceID)
	if err != nil {
		t.Fatalf("list connections: %v", err)
	}
	if len(conns) != 0 {
		t.Errorf("connections = %d after the delete, want 0", len(conns))
	}
	if code, _ := do(t, api.owner, "GET", api.url+"/api/v1/tinvest/connections/"+id.String(), ""); code != http.StatusNotFound {
		t.Errorf("the deleted connection still answers %d, want 404", code)
	}
}

// -------------------------------------------------------------------------
// syncing now
// -------------------------------------------------------------------------

func TestSyncingNowQueuesOneAndSaysSoWhenItDidNot(t *testing.T) {
	api := newTestAPI(t)
	id, _ := api.createConnection(t)
	url := api.url + "/api/v1/tinvest/connections/" + id.String() + "/sync"

	code, body := do(t, api.owner, "POST", url, "")
	if code != http.StatusAccepted || !strings.Contains(body, `"queued":true`) {
		t.Fatalf("status %d, body %s; want 202 and queued=true", code, body)
	}
	queued := api.inserter.queued()
	// The first import queued one already, so this is the second.
	if len(queued) != 2 || queued[1].Trigger != string(TriggerManual) {
		t.Fatalf("queued %+v, want a second one with trigger %q", queued, TriggerManual)
	}

	api.inserter.dup = true
	code, body = do(t, api.owner, "POST", url, "")
	if code != http.StatusAccepted || !strings.Contains(body, `"queued":false`) {
		t.Errorf("status %d, body %s; want 202 and queued=false when one is already in the queue", code, body)
	}
}

func TestSyncingNowRefusesAConnectionTheSchedulerWouldSkip(t *testing.T) {
	api := newTestAPI(t)
	ctx := context.Background()
	id, _ := api.createConnection(t)
	url := api.url + "/api/v1/tinvest/connections/" + id.String() + "/sync"

	for _, status := range []ConnectionStatus{StatusDisabled, StatusTokenRevoked} {
		if err := api.store.UpdateConnectionStatus(ctx, id, status); err != nil {
			t.Fatalf("set %q: %v", status, err)
		}
		before := len(api.inserter.queued())
		code, body := do(t, api.owner, "POST", url, "")
		if code != http.StatusConflict {
			t.Errorf("sync on %q: status %d (body %s), want 409", status, code, body)
		}
		if got := len(api.inserter.queued()); got != before {
			t.Errorf("sync on %q queued a job anyway (%d, was %d)", status, got, before)
		}
	}
}

// -------------------------------------------------------------------------
// paging
// -------------------------------------------------------------------------

// seedRunsAndUnparsed writes runs finished ok and mirror rows the projection
// could not read, through the very stores the sync worker writes them with.
func (a *testAPI) seedRunsAndUnparsed(t *testing.T, connID uuid.UUID, runs, unparsed int) {
	t.Helper()
	ctx := context.Background()
	links, err := a.store.LinksByConnection(ctx, connID)
	if err != nil || len(links) == 0 {
		t.Fatalf("links = %d, %v", len(links), err)
	}
	link := links[0]

	items := make([]OperationItem, 0, unparsed)
	for i := range unparsed {
		items = append(items, OperationItem{
			ID:      "op-" + string(rune('a'+i)),
			Type:    "OPERATION_TYPE_WRITING_OFF_VARMARGIN",
			State:   "OPERATION_STATE_EXECUTED",
			Date:    time.Date(2026, 7, 1+i, 12, 0, 0, 0, time.UTC),
			Payment: MoneyValue{Currency: "RUB", Units: int64(-100 - i), Nano: 0},
			Raw:     json.RawMessage(`{"id":"op-` + string(rune('a'+i)) + `"}`),
		})
	}
	if len(items) > 0 {
		if _, err := a.store.SyncMirror(ctx, connID, link, items, time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)); err != nil {
			t.Fatalf("seed mirror: %v", err)
		}
		rows, err := a.store.MirrorRowsByLink(ctx, link.ID)
		if err != nil {
			t.Fatalf("read mirror: %v", err)
		}
		reasons := map[uuid.UUID]string{}
		for _, r := range rows {
			reasons[r.ID] = string(ReasonUnsupportedType)
		}
		if err := a.store.SetUnparsedReasons(ctx, reasons); err != nil {
			t.Fatalf("seed unparsed reasons: %v", err)
		}
	}

	for i := range runs {
		run, err := a.store.StartRun(ctx, connID, link.ID, TriggerSchedule)
		if err != nil {
			t.Fatalf("start run: %v", err)
		}
		outcome := RunOutcome{Status: RunOK, ReadCount: unparsed, AddedCount: unparsed, UnparsedCount: unparsed}
		if i == runs-1 {
			outcome.Reconcile = ReconcileResult{
				Status: ReconcileMismatched,
				Mismatches: []ReconcileMismatch{{
					Kind: MismatchCurrency, Label: "RUB",
					Broker: decimal.RequireFromString("1000.50"), Journal: decimal.Zero,
				}},
			}
		}
		if err := a.store.FinishRun(ctx, run.ID, outcome); err != nil {
			t.Fatalf("finish run: %v", err)
		}
	}
}

func TestBothListsSayWhetherThereIsMoreBehindThePage(t *testing.T) {
	api := newTestAPI(t)
	id, _ := api.createConnection(t)
	api.seedRunsAndUnparsed(t, id, 3, 3)
	conn := api.url + "/api/v1/tinvest/connections/" + id.String()

	for _, c := range []struct {
		what, path, field string
	}{
		{"runs", conn + "/runs", "runs"},
		{"unparsed", conn + "/unparsed", "operations"},
	} {
		var page struct {
			HasMore bool              `json:"has_more"`
			Runs    []json.RawMessage `json:"runs"`
			Ops     []json.RawMessage `json:"operations"`
		}
		code, body := do(t, api.owner, "GET", c.path+"?limit=2", "")
		if code != http.StatusOK {
			t.Fatalf("%s: status %d, body %s", c.what, code, body)
		}
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatalf("%s: decode %s: %v", c.what, body, err)
		}
		got := len(page.Runs) + len(page.Ops)
		if got != 2 || !page.HasMore {
			t.Errorf("%s page of 2 out of 3: %d items, has_more=%v; want 2 and true", c.what, got, page.HasMore)
		}

		code, body = do(t, api.owner, "GET", c.path+"?limit=2&offset=2", "")
		if code != http.StatusOK {
			t.Fatalf("%s: status %d, body %s", c.what, code, body)
		}
		page.Runs, page.Ops = nil, nil
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatalf("%s: decode %s: %v", c.what, body, err)
		}
		if got := len(page.Runs) + len(page.Ops); got != 1 || page.HasMore {
			t.Errorf("%s last page: %d items, has_more=%v; want 1 and false", c.what, got, page.HasMore)
		}
	}
}

func TestAPageBeyondTheCeilingIsRefusedAndNotQuietlyShortened(t *testing.T) {
	api := newTestAPI(t)
	id, _ := api.createConnection(t)
	conn := api.url + "/api/v1/tinvest/connections/" + id.String()

	for _, path := range []string{conn + "/runs", conn + "/unparsed"} {
		for _, q := range []string{
			"?limit=201", "?limit=0", "?limit=-1", "?limit=many", "?offset=-1", "?offset=x",
		} {
			code, body := do(t, api.owner, "GET", path+q, "")
			if code != http.StatusBadRequest {
				t.Errorf("GET %s%s: status %d (body %s), want 400 — a ceiling the server "+
					"silently clamps to is #118 all over again", path, q, code, body)
			}
		}
		// And the maximum itself is accepted, so the refusal above is about the
		// bound rather than about the parameter existing at all.
		if code, body := do(t, api.owner, "GET", path+"?limit=200", ""); code != http.StatusOK {
			t.Errorf("GET %s?limit=200: status %d (body %s), want 200", path, code, body)
		}
	}
}

// -------------------------------------------------------------------------
// what a run and a connection publish about the check against the broker
// -------------------------------------------------------------------------

func TestARunNobodyCheckedIsNotARunThatFoundNothing(t *testing.T) {
	api := newTestAPI(t)
	ctx := context.Background()
	id, _ := api.createConnection(t)
	links, err := api.store.LinksByConnection(ctx, id)
	if err != nil || len(links) != 1 {
		t.Fatalf("links = %d, %v", len(links), err)
	}

	// Two runs: an older one that reconciled and agreed, and a newer one that
	// failed before it could check anything.
	matched, err := api.store.StartRun(ctx, id, links[0].ID, TriggerInitial)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if err := api.store.FinishRun(ctx, matched.ID, RunOutcome{
		Status: RunOK, Reconcile: ReconcileResult{Status: ReconcileMatched},
	}); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	failed, err := api.store.StartRun(ctx, id, links[0].ID, TriggerSchedule)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if err := api.store.FinishRun(ctx, failed.ID, RunOutcome{
		Status: RunFailed, Error: "the broker said no",
	}); err != nil {
		t.Fatalf("finish run: %v", err)
	}

	var page struct {
		Runs []struct {
			ID              uuid.UUID         `json:"id"`
			LinkID          uuid.UUID         `json:"link_id"`
			Status          string            `json:"status"`
			ReconcileStatus string            `json:"reconcile_status"`
			ReconciledAt    *string           `json:"reconciled_at"`
			Mismatches      []json.RawMessage `json:"mismatches"`
		} `json:"runs"`
	}
	code, body := do(t, api.owner, "GET", api.url+"/api/v1/tinvest/connections/"+id.String()+"/runs", "")
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if len(page.Runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(page.Runs))
	}
	newest, oldest := page.Runs[0], page.Runs[1]
	if newest.ID != failed.ID {
		t.Fatalf("the log is not newest first: %+v", page.Runs)
	}
	if newest.ReconcileStatus != "not_checked" || newest.ReconciledAt != nil {
		t.Errorf("the failed run says %q at %v, want not_checked and no date",
			newest.ReconcileStatus, newest.ReconciledAt)
	}
	if oldest.ReconcileStatus != "matched" || oldest.ReconciledAt == nil {
		t.Errorf("the run that agreed says %q at %v, want matched with a date",
			oldest.ReconcileStatus, oldest.ReconciledAt)
	}
	if len(newest.Mismatches) != 0 || len(oldest.Mismatches) != 0 {
		t.Errorf("mismatches: failed=%d matched=%d, want 0 and 0", len(newest.Mismatches), len(oldest.Mismatches))
	}
	if newest.LinkID != links[0].ID {
		t.Errorf("link_id = %s, want the connection's only link %s", newest.LinkID, links[0].ID)
	}

	// And the connection keeps the verdict the older run reached, rather than
	// reporting the newest run's silence as "nobody has checked".
	var conn struct {
		LastReconcile *struct {
			At         time.Time         `json:"at"`
			Status     string            `json:"status"`
			Mismatches []json.RawMessage `json:"mismatches"`
		} `json:"last_reconcile"`
	}
	code, body = do(t, api.owner, "GET", api.url+"/api/v1/tinvest/connections/"+id.String(), "")
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &conn); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if conn.LastReconcile == nil {
		t.Fatalf("last_reconcile is null, but a check did happen: %s", body)
	}
	if conn.LastReconcile.Status != "matched" || len(conn.LastReconcile.Mismatches) != 0 {
		t.Errorf("last_reconcile = %+v, want the matched verdict with an empty list", conn.LastReconcile)
	}
}

func TestAConnectionThatWasNeverCheckedSaysSoWithNull(t *testing.T) {
	api := newTestAPI(t)
	id, _ := api.createConnection(t)
	var conn struct {
		LastReconcile        json.RawMessage `json:"last_reconcile"`
		LastSuccessfulSyncAt json.RawMessage `json:"last_successful_sync_at"`
	}
	_, body := do(t, api.owner, "GET", api.url+"/api/v1/tinvest/connections/"+id.String(), "")
	if err := json.Unmarshal([]byte(body), &conn); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if string(conn.LastReconcile) != "null" || string(conn.LastSuccessfulSyncAt) != "null" {
		t.Errorf("last_reconcile=%s last_successful_sync_at=%s, want both an explicit null",
			conn.LastReconcile, conn.LastSuccessfulSyncAt)
	}
}

func TestTheUnparsedListCarriesTheBrokersOwnRecord(t *testing.T) {
	api := newTestAPI(t)
	id, _ := api.createConnection(t)
	api.seedRunsAndUnparsed(t, id, 1, 1)

	var page struct {
		Operations []struct {
			ID         uuid.UUID       `json:"id"`
			OccurredAt time.Time       `json:"occurred_at"`
			OpType     string          `json:"op_type"`
			Payment    string          `json:"payment"`
			Currency   string          `json:"currency"`
			Reason     string          `json:"reason"`
			Raw        json.RawMessage `json:"raw"`
		} `json:"operations"`
	}
	code, body := do(t, api.owner, "GET",
		api.url+"/api/v1/tinvest/connections/"+id.String()+"/unparsed", "")
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if len(page.Operations) != 1 {
		t.Fatalf("operations = %d, want 1", len(page.Operations))
	}
	op := page.Operations[0]
	if op.OpType != "OPERATION_TYPE_WRITING_OFF_VARMARGIN" || op.Reason != "unsupported_type" {
		t.Errorf("op_type=%q reason=%q, want the broker's own word and the code for it", op.OpType, op.Reason)
	}
	if op.Payment != "-100" || op.Currency != "RUB" {
		t.Errorf("payment=%q currency=%q, want -100 RUB exactly as the broker gave it", op.Payment, op.Currency)
	}
	// Compared as a document rather than as bytes: the column is jsonb, which
	// normalizes whitespace and key order on the way in (see MirrorRow.Raw), so
	// a byte comparison here would be a test of PostgreSQL's formatting.
	var raw map[string]any
	if err := json.Unmarshal(op.Raw, &raw); err != nil {
		t.Fatalf("raw is not the broker's own document: %s: %v", op.Raw, err)
	}
	if raw["id"] != "op-a" {
		t.Errorf("raw = %s, want the broker's own element for op-a", op.Raw)
	}
}

// -------------------------------------------------------------------------
// one space cannot reach another's connection
// -------------------------------------------------------------------------

func TestAnUnknownConnectionIsNotFoundRatherThanForbidden(t *testing.T) {
	api := newTestAPI(t)
	stranger := uuid.New().String()
	for _, e := range []endpoint{
		{"GET", "/api/v1/tinvest/connections/" + stranger, ""},
		{"PATCH", "/api/v1/tinvest/connections/" + stranger, `{"status":"disabled"}`},
		{"DELETE", "/api/v1/tinvest/connections/" + stranger, ""},
		{"POST", "/api/v1/tinvest/connections/" + stranger + "/sync", ""},
		{"GET", "/api/v1/tinvest/connections/" + stranger + "/runs", ""},
		{"GET", "/api/v1/tinvest/connections/" + stranger + "/unparsed", ""},
	} {
		if code, body := do(t, api.owner, e.method, api.url+e.path, e.body); code != http.StatusNotFound {
			t.Errorf("%s %s: status %d (body %s), want 404", e.method, e.path, code, body)
		}
	}
	if code, _ := do(t, api.owner, "GET", api.url+"/api/v1/tinvest/connections/not-a-uuid", ""); code != http.StatusBadRequest {
		t.Errorf("a malformed id: status %d, want 400", code)
	}
}
