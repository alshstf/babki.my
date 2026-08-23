package tinvest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
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
	url       string
	owner     *http.Client
	editor    *http.Client
	viewer    *http.Client
	pool      *pgxpool.Pool
	srv       *httpserver.Server
	store     *Store
	accounts  *account.Store
	broker    *fakeBroker
	brokerURL string
	box       *secretbox.Box
	inserter  *fakeInserter
	spaceID   uuid.UUID
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
	svc := NewService(store, accStore, operation.NewService(operation.NewStore(pool)), box,
		func(token string) (*Client, error) {
			return NewClient(brokerSrv.Client(), brokerSrv.URL, token, slog.Default()), nil
		}, inserter, slog.Default())

	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(famSvc, famStore, auth, sm).Mount(srv)
	NewHandler(svc, auth, sm).Mount(srv)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	api := &testAPI{
		url: ts.URL, pool: pool, srv: srv, store: store, accounts: accStore,
		broker: broker, brokerURL: brokerSrv.URL, box: box,
		inserter: inserter, spaceID: ownerP.SpaceID,
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

// endpoint is one request to this module: a method, a path with the connection
// id already filled in, and a body the handler will accept.
type endpoint struct {
	method, path, body string
}

// tinvestPathPrefix is what tells this module's routes from the rest of the
// server's in mountedRoutes below.
const tinvestPathPrefix = "/api/v1/tinvest/"

// mountedRoutes is every route THIS MODULE mounted, taken from the router the
// test server was built with. See httpserver.Server.Routes: the patterns come
// back with their method attached, which is how net/http spells a route and how
// guardedRequests keys them.
func (a *testAPI) mountedRoutes(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, pattern := range a.srv.Routes() {
		_, path, ok := strings.Cut(pattern, " ")
		if !ok {
			continue // no method in the pattern: not one of this module's
		}
		if strings.HasPrefix(path, tinvestPathPrefix) {
			out = append(out, pattern)
		}
	}
	return out
}

// guardedRequests is one request per route this module mounts, keyed by the
// pattern Handler.Mount registers that route under.
//
// IT IS NOT THE LIST OF ROUTES UNDER TEST, and the difference is the whole point
// of the arrangement. That list comes from the router (mountedRoutes above): a
// list of routes typed into a test goes on looking complete the day another
// route is mounted without a line added here, and the one route nobody thought
// about would be the one nobody checked. What this map adds is the only part a
// router cannot supply — a body the handler will get past its own decoding, and
// a real connection id in place of the {connectionId} the pattern carries. A
// route mounted with no entry here has no request to send, and the test says so
// and fails rather than skipping it.
//
// The method is NOT repeated in the value: it is read off the key, so the two
// cannot come to disagree.
func guardedRequests(id uuid.UUID) map[string]struct{ path, body string } {
	conn := "/api/v1/tinvest/connections/" + id.String()
	return map[string]struct{ path, body string }{
		"POST /api/v1/tinvest/token-check": {"/api/v1/tinvest/token-check", `{"token":"` + demoToken + `"}`},
		"GET /api/v1/tinvest/connections":  {"/api/v1/tinvest/connections", ""},
		"POST /api/v1/tinvest/connections": {
			"/api/v1/tinvest/connections",
			`{"token":"` + demoToken + `","accounts":[{"broker_account_id":"2000000002","account_name":"ИИС"}]}`,
		},
		"GET /api/v1/tinvest/connections/{connectionId}":          {conn, ""},
		"PATCH /api/v1/tinvest/connections/{connectionId}":        {conn, `{"status":"disabled"}`},
		"DELETE /api/v1/tinvest/connections/{connectionId}":       {conn, ""},
		"POST /api/v1/tinvest/connections/{connectionId}/sync":    {conn + "/sync", ""},
		"GET /api/v1/tinvest/connections/{connectionId}/runs":     {conn + "/runs", ""},
		"GET /api/v1/tinvest/connections/{connectionId}/unparsed": {conn + "/unparsed", ""},
	}
}

func TestEveryEndpointRefusesAnEditorAndAViewer(t *testing.T) {
	api := newTestAPI(t)
	id, _ := api.createConnection(t)

	mounted := api.mountedRoutes(t)
	// Not a count of routes — that is exactly what this test must not state for
	// itself — but proof that the filter matched the module at all. A prefix that
	// matched nothing would leave the loop below with nothing to do, and a test
	// that checks nothing passes.
	if len(mounted) == 0 {
		t.Fatalf("no route of this module was found among the router's %v", api.srv.Routes())
	}

	requests := guardedRequests(id)
	for _, pattern := range mounted {
		method, _, _ := strings.Cut(pattern, " ")
		req, ok := requests[pattern]
		if !ok {
			t.Fatalf("%q is mounted and no request for it is written down: add one to "+
				"guardedRequests, so this test can watch that route refuse an editor and a viewer",
				pattern)
		}
		for _, who := range []struct {
			name string
			c    *http.Client
		}{{"editor", api.editor}, {"viewer", api.viewer}} {
			code, body := do(t, who.c, method, api.url+req.path, req.body)
			if code != http.StatusForbidden {
				t.Errorf("%s as %s: status %d (body %s), want 403", pattern, who.name, code, body)
			}
		}
	}
	// And the other way round, so a route removed from Mount does not leave a
	// request here quietly addressing nothing.
	for pattern := range requests {
		if !slices.Contains(mounted, pattern) {
			t.Errorf("guardedRequests names %q, which Handler.Mount does not mount", pattern)
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
		LastSuccessfulSyncAt *string            `json:"last_successful_sync_at"`
		Reconciles           []accountReconcile `json:"reconciles"`
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
	if out.LastSuccessfulSyncAt != nil {
		t.Errorf("a brand new connection claims a sync at %v; it has had none", out.LastSuccessfulSyncAt)
	}
	// A verdict for each account it imports, and every one of them saying that
	// nobody has checked yet — which is what a brand new connection knows.
	if len(out.Reconciles) != 2 {
		t.Fatalf("verdicts = %d, want one per linked account (2)", len(out.Reconciles))
	}
	for i, rec := range out.Reconciles {
		if rec.Status != "not_checked" || rec.At != nil {
			t.Errorf("account %d claims a check: %+v", i, rec)
		}
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
	// mustSay is set where the status code alone cannot tell this refusal from
	// the one that would answer the same request if this one were gone.
	for _, c := range []struct {
		name, body string
		want       int
		mustSay    string
	}{
		{
			// 422 AND NOT 400, and the difference is the whole reason this row
			// is here twice over: the token in this request is the demo token,
			// which works, and the body is exactly the shape the contract
			// declares. What is wrong is the account, and a client told 400
			// would caption it as a refused token and send the owner to
			// re-issue one that never stopped working (see writeError).
			"an account the token cannot see",
			`{"token":"` + demoToken + `","accounts":[{"broker_account_id":"9999","account_name":"X"}]}`,
			http.StatusUnprocessableEntity, "",
		},
		{
			"an account of a kind this program does not import",
			`{"token":"` + demoToken + `","accounts":[{"broker_account_id":"2000000003","account_name":"X"}]}`,
			http.StatusUnprocessableEntity, "",
		},
		{
			"the same broker account twice",
			`{"token":"` + demoToken + `","accounts":[` +
				`{"broker_account_id":"2000000001","account_name":"A"},` +
				`{"broker_account_id":"2000000001","account_name":"B"}]}`,
			http.StatusBadRequest, "",
		},
		{
			"no accounts at all",
			`{"token":"` + demoToken + `","accounts":[]}`, http.StatusBadRequest, "",
		},
		{
			"an empty account name",
			`{"token":"` + demoToken + `","accounts":[{"broker_account_id":"2000000001","account_name":""}]}`,
			http.StatusBadRequest, "",
		},
		{
			// The REASON and not just the code: an empty broker account id is
			// also one the token cannot see, so deleting validatePicks' refusal
			// of it would leave the next check answering — with the same 400, and
			// with a sentence telling the owner to pick a different account
			// rather than that they sent an empty field. A test reading the
			// status alone would stay green through that deletion; this one does
			// not, and the emptiness is a bound the contract publishes
			// (TinvestAccountPick.broker_account_id, minLength 1).
			"an empty broker account id",
			`{"token":"` + demoToken + `","accounts":[{"broker_account_id":"","account_name":"X"}]}`,
			http.StatusBadRequest, "broker_account_id must not be empty",
		},
		{
			"an empty token",
			`{"token":"","accounts":[{"broker_account_id":"2000000001","account_name":"X"}]}`,
			http.StatusBadRequest, "",
		},
	} {
		code, body := do(t, api.owner, "POST", base, c.body)
		if code != c.want {
			t.Errorf("%s: status %d (body %s), want %d", c.name, code, body, c.want)
		}
		if c.mustSay != "" && !strings.Contains(body, c.mustSay) {
			t.Errorf("%s: body %s, want it to say %q", c.name, body, c.mustSay)
		}
	}

	// Nothing was written by any of them: a refusal that had already created an
	// account would leave the accounts screen holding a stranger.
	accounts, err := api.accounts.ListWithBalance(context.Background(), api.spaceID)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(accounts) != 0 {
		t.Errorf("accounts = %d after seven refusals, want 0", len(accounts))
	}
	if got := api.inserter.queued(); len(got) != 0 {
		t.Errorf("queued %d syncs after seven refusals, want 0", len(got))
	}
}

// TestCreateTellsARefusedTokenFromAnAccountItCannotImport is the pair a client
// branches on, both legs of it on ONE endpoint and with everything else held
// still. It is the whole reason ErrBrokerAccountNotImportable stopped sharing
// ErrTokenRejected's 400.
//
// Why the second leg is not a made-up case: creating a connection asks the
// broker for its account list AFRESH, and that list is a live answer. An account
// closed between the wizard's token-check and its create, or a token whose
// access was narrowed, produces exactly this — a request whose token still
// works, naming an account the broker no longer offers. Under one status code
// the client had to caption that as a refused token and send the owner off to
// re-issue a working one.
func TestCreateTellsARefusedTokenFromAnAccountItCannotImport(t *testing.T) {
	api := newTestAPI(t)
	base := api.url + "/api/v1/tinvest/connections"
	pick := func(id string) string {
		return `{"token":"` + demoToken + `","accounts":[{"broker_account_id":"` + id +
			`","account_name":"X"}]}`
	}

	// The broker refuses the token. The account picked is a real, importable
	// one, so the ONLY thing wrong is the token.
	api.broker.set(http.StatusUnauthorized, `{"code":16,"message":"unauthenticated","description":"40003"}`)
	if code, body := do(t, api.owner, "POST", base, pick("2000000001")); code != http.StatusBadRequest {
		t.Errorf("a refused token: status %d (body %s), want 400", code, body)
	}

	// The same endpoint, the same token, the broker answering normally again.
	// Now the token works and the request is well formed, and the only thing
	// wrong is the account — which must NOT be answered with the token's code.
	api.broker.set(0, "")
	code, body := do(t, api.owner, "POST", base, pick("9999"))
	if code == http.StatusBadRequest {
		t.Fatalf("an account the token cannot see: status 400 (body %s) — the same answer as a "+
			"refused token, which leaves a client telling the owner to re-issue a token that works", body)
	}
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("an account the token cannot see: status %d (body %s), want 422", code, body)
	}
}

// TestAnEmptyTokenIsRefusedBeforeTheBrokerIsAsked covers the other end of the
// same class: the token check's own refusal of an empty token. The broker
// refusing a token is a 400 as well (see
// TestARefusedTokenAndAnUnreachableBrokerAreDifferentAnswers), so the status
// code cannot tell the two apart — what can is that this one never left the
// process.
func TestAnEmptyTokenIsRefusedBeforeTheBrokerIsAsked(t *testing.T) {
	api := newTestAPI(t)

	code, body := do(t, api.owner, "POST", api.url+"/api/v1/tinvest/token-check", `{"token":""}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status %d (body %s), want 400", code, body)
	}
	if !strings.Contains(body, "token must not be empty") {
		t.Errorf("body = %s, want it to name the empty token rather than the broker's verdict", body)
	}
	if seen := api.broker.seen(); len(seen) != 0 {
		t.Errorf("the broker was asked %v, want not to have been asked at all: an empty token is "+
			"refused here, and sending it would be one pointless call per empty field", seen)
	}
}

// strayAccountCreator answers with an account that exists nowhere, which makes
// the NEXT write — the link onto it — fail with ErrLinkOutsideSpace. That is a
// failure in the middle of the writes rather than before them, which is the
// only way to reach the state the test below is about.
type strayAccountCreator struct{}

func (strayAccountCreator) Create(_ context.Context, _ uuid.UUID, _ *uuid.UUID,
	name string, _ account.Type, _, _ string,
) (account.Account, error) {
	return account.Account{ID: uuid.New(), Name: name}, nil
}

// TestAHalfBuiltConnectionThatCouldNotBeRemovedIsNotScheduled is the fourth
// outcome CreateConnection's doc names: a write fails, and the compensating
// removal fails too (it is a best effort — undoConnection logs its own failure
// and returns the original one). What survives must be a connection the
// scheduler passes over, because an active one would be synced hourly, with a
// working token, into accounts the owner was told had not been made.
//
// Read through ListActiveConnections and not through the space's own list: that
// is the read the hourly dispatcher makes, and it is the one whose answer
// decides whether the leftover is dangerous.
func TestAHalfBuiltConnectionThatCouldNotBeRemovedIsNotScheduled(t *testing.T) {
	api := newTestAPI(t)
	ctx := context.Background()

	// The cleanup cannot succeed: the DELETE is refused by the database itself.
	// Nothing in the schema produces that on its own, and a stubbed store would
	// prove only that a stub was called.
	for _, stmt := range []string{
		`CREATE FUNCTION refuse_the_delete() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION 'the cleanup cannot remove this connection'; END $$`,
		`CREATE TRIGGER refuse_connection_delete BEFORE DELETE ON tinvest_connections
			FOR EACH ROW EXECUTE FUNCTION refuse_the_delete()`,
	} {
		if _, err := api.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("install the refusing trigger: %v", err)
		}
	}

	svc := NewService(api.store, strayAccountCreator{}, operation.NewService(operation.NewStore(api.pool)), api.box,
		func(token string) (*Client, error) {
			return NewClient(http.DefaultClient, api.brokerURL, token, slog.Default()), nil
		}, api.inserter, slog.New(&logCapture{}))

	_, err := svc.CreateConnection(ctx,
		family.Principal{SpaceID: api.spaceID, Role: family.RoleOwner}, demoToken,
		[]AccountPick{{BrokerAccountID: "2000000001", AccountName: "Брокерский"}})
	// The failure is the one this test set up — a link that could not be written
	// — and not something that happened before the first write, which would make
	// the state below prove nothing.
	if !errors.Is(err, ErrLinkOutsideSpace) {
		t.Fatalf("CreateConnection failed with %v, want %v", err, ErrLinkOutsideSpace)
	}

	// It did survive — otherwise this would be a test of the cleanup working.
	all, err := api.store.ListConnections(ctx, api.spaceID)
	if err != nil {
		t.Fatalf("list connections: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("connections = %d, want the half-built one left behind by the refused delete", len(all))
	}
	if all[0].Status != StatusDisabled {
		t.Errorf("the leftover connection is %q, want %q", all[0].Status, StatusDisabled)
	}

	active, err := api.store.ListActiveConnections(ctx)
	if err != nil {
		t.Fatalf("list active connections: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("the hourly dispatcher sees %d connection(s), want 0: a half-built connection "+
			"it can see is one it imports from, into accounts the owner was told were not created", len(active))
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

// seedImportedOperation writes one journal operation of the kind the import
// writes — this importer's own source, on the account a link feeds — through
// the journal's own store. Without it the test below would check half of its
// own title: an account with no operations in it says nothing about whether
// operations survive a disconnect.
func (a *testAPI) seedImportedOperation(t *testing.T, accountID uuid.UUID) uuid.UUID {
	t.Helper()
	externalID := "seeded-operation"
	op, err := operation.NewStore(a.pool).Create(context.Background(), a.spaceID, operation.Operation{
		AccountID:   accountID,
		Type:        operation.TypeDeposit,
		OccurredOn:  time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
		AmountMinor: 250_000,
		Currency:    "RUB",
		Source:      Source,
		ExternalID:  &externalID,
	}, nil)
	if err != nil {
		t.Fatalf("seed an imported operation: %v", err)
	}
	return op.ID
}

func TestDeletingAConnectionLeavesTheAccountsAndTheirOperations(t *testing.T) {
	api := newTestAPI(t)
	ctx := context.Background()
	id, _ := api.createConnection(t)
	before, err := api.accounts.ListWithBalance(ctx, api.spaceID)
	if err != nil || len(before) != 1 {
		t.Fatalf("accounts before = %d, %v; want 1", len(before), err)
	}
	opID := api.seedImportedOperation(t, before[0].ID)

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
	// And what the import wrote INTO that account is the owner's data too. The
	// migration's cascades reach the mirror, the links and the run log; they must
	// not reach the journal, which holds no foreign key back to the connection.
	ops, _, err := operation.NewStore(api.pool).ListByAccount(ctx, api.spaceID, before[0].ID, 10, 0)
	if err != nil {
		t.Fatalf("list operations: %v", err)
	}
	if len(ops) != 1 || ops[0].ID != opID {
		t.Errorf("operations after the disconnect = %+v, want the one the import wrote (%s): "+
			"the journal is the owner's record of what happened, not the connection's", ops, opID)
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
		verdicts := map[uuid.UUID]UnparsedVerdict{}
		for _, r := range rows {
			verdicts[r.ID] = UnparsedVerdict{Reason: string(ReasonUnsupportedType)}
		}
		if err := a.store.SetUnparsedVerdicts(ctx, verdicts); err != nil {
			t.Fatalf("seed unparsed verdicts: %v", err)
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

// TestUnparsedOperationsCarryTheWordsOfWhatRefusedThem: the code alone was the
// whole answer for 134 of the owner's rows, and «Операцию отклонил движок
// журнала» is the same sentence over a sale with nothing behind it, an amount
// the journal will not hold and a transfer whose other leg failed. The detail
// is what tells them apart, so it has to travel the whole way — mirror column,
// service, response body — and not stop at a log line.
//
// The empty case is asserted from the SAME response and matters as much: the
// field is required by the contract, so a row with nothing written down must
// come back as "" rather than be left out, which is what a client rendering it
// conditionally depends on.
func TestUnparsedOperationsCarryTheWordsOfWhatRefusedThem(t *testing.T) {
	api := newTestAPI(t)
	id, _ := api.createConnection(t)
	ctx := context.Background()
	links, err := api.store.LinksByConnection(ctx, id)
	if err != nil || len(links) == 0 {
		t.Fatalf("links = %d, %v", len(links), err)
	}
	link := links[0]

	const detail = "operation: selling 100 units leaves the position at -40"
	items := []OperationItem{
		{
			ID: "op-refused", Type: "OPERATION_TYPE_SELL", State: "OPERATION_STATE_EXECUTED",
			Date:    time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC),
			Payment: MoneyValue{Currency: "RUB", Units: 100}, Raw: json.RawMessage(`{"id":"op-refused"}`),
		},
		{
			ID: "op-silent", Type: "OPERATION_TYPE_SELL", State: "OPERATION_STATE_EXECUTED",
			Date:    time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
			Payment: MoneyValue{Currency: "RUB", Units: 200}, Raw: json.RawMessage(`{"id":"op-silent"}`),
		},
	}
	if _, err := api.store.SyncMirror(ctx, id, link, items, time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	rows, err := api.store.MirrorRowsByLink(ctx, link.ID)
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	verdicts := map[uuid.UUID]UnparsedVerdict{}
	for _, r := range rows {
		v := UnparsedVerdict{Reason: string(ReasonEngineRefused)}
		if r.BrokerOperationID == "op-refused" {
			v.Detail = detail
		}
		verdicts[r.ID] = v
	}
	if err := api.store.SetUnparsedVerdicts(ctx, verdicts); err != nil {
		t.Fatalf("seed unparsed verdicts: %v", err)
	}

	code, body := do(t, api.owner, "GET",
		api.url+"/api/v1/tinvest/connections/"+id.String()+"/unparsed", "")
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	var page struct {
		Operations []struct {
			OpType string  `json:"op_type"`
			Reason string  `json:"reason"`
			Detail *string `json:"detail"`
		} `json:"operations"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if len(page.Operations) != 2 {
		t.Fatalf("the list holds %d operations, want 2 (%s)", len(page.Operations), body)
	}
	// Newest first: op-refused happened on the 2nd, op-silent on the 1st.
	for i, want := range []string{detail, ""} {
		got := page.Operations[i]
		if got.Reason != string(ReasonEngineRefused) {
			t.Errorf("operation %d: reason %q, want %q", i, got.Reason, ReasonEngineRefused)
		}
		// A pointer, so that "the field was left out" is a different answer
		// from "the field is empty" — the contract requires it either way.
		if got.Detail == nil {
			t.Fatalf("operation %d carries no detail field at all: %s", i, body)
		}
		if *got.Detail != want {
			t.Errorf("operation %d: detail %q, want %q", i, *got.Detail, want)
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

	// And the account keeps the verdict the older run of it reached, rather
	// than reporting the newest run's silence as "nobody has checked".
	conn := connectionReconciles(t, api, id)
	if len(conn) != 1 {
		t.Fatalf("%d verdicts for a connection with one account: %+v", len(conn), conn)
	}
	if conn[0].Status != "matched" || len(conn[0].Mismatches) != 0 || conn[0].At == nil {
		t.Errorf("the account says %+v, want the matched verdict with an empty list and a time", conn[0])
	}
	if conn[0].LinkID != links[0].ID {
		t.Errorf("the verdict names link %s, want %s", conn[0].LinkID, links[0].ID)
	}
}

// accountReconcile is TinvestAccountReconcile as the wire carries it, decoded
// by hand: what this file checks is the JSON the owner's browser receives, not
// the Go value the handler assembled.
type accountReconcile struct {
	LinkID            uuid.UUID         `json:"link_id"`
	AccountID         uuid.UUID         `json:"account_id"`
	BrokerAccountName string            `json:"broker_account_name"`
	At                *time.Time        `json:"at"`
	Status            string            `json:"status"`
	Mismatches        []json.RawMessage `json:"mismatches"`
}

func connectionReconciles(t *testing.T, api *testAPI, id uuid.UUID) []accountReconcile {
	t.Helper()
	code, body := do(t, api.owner, "GET", api.url+"/api/v1/tinvest/connections/"+id.String(), "")
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	var conn struct {
		Reconciles []accountReconcile `json:"reconciles"`
	}
	if err := json.Unmarshal([]byte(body), &conn); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return conn.Reconciles
}

// THE CASE A SINGLE CONNECTION-WIDE VERDICT GOT WRONG. Two accounts checked in
// one sync: the first differs, the second agrees a moment later. Publishing the
// connection's newest check drew a tick over the difference, and — because the
// differing account's check is forever the older of the two — its verdict could
// never be seen at all.
func TestEachLinkedAccountCarriesItsOwnVerdict(t *testing.T) {
	api := newTestAPI(t)
	ctx := context.Background()
	code, body := do(t, api.owner, "POST", api.url+"/api/v1/tinvest/connections",
		`{"token":"`+demoToken+`","accounts":[`+
			`{"broker_account_id":"2000000001","account_name":"Т-Банк брокерский"},`+
			`{"broker_account_id":"2000000002","account_name":"Т-Банк ИИС"}]}`)
	if code != http.StatusCreated {
		t.Fatalf("create connection: status %d, body %s", code, body)
	}
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	links, err := api.store.LinksByConnection(ctx, created.ID)
	if err != nil || len(links) != 2 {
		t.Fatalf("links = %d, %v", len(links), err)
	}

	finish := func(linkID uuid.UUID, rec ReconcileResult) {
		t.Helper()
		run, err := api.store.StartRun(ctx, created.ID, linkID, TriggerSchedule)
		if err != nil {
			t.Fatalf("start run: %v", err)
		}
		if err := api.store.FinishRun(ctx, run.ID, RunOutcome{Status: RunOK, Reconcile: rec}); err != nil {
			t.Fatalf("finish run: %v", err)
		}
	}
	finish(links[0].ID, ReconcileResult{
		Status: ReconcileMismatched,
		Mismatches: []ReconcileMismatch{{
			Kind: MismatchCurrency, Label: "RUB",
			Broker: decimal.RequireFromString("1000.50"), Journal: decimal.Zero,
		}},
	})
	finish(links[1].ID, ReconcileResult{Status: ReconcileMatched})

	got := connectionReconciles(t, api, created.ID)
	if len(got) != 2 {
		t.Fatalf("%d verdicts for 2 accounts: %+v", len(got), got)
	}
	// One entry per linked account, in the order the accounts are listed in.
	if got[0].LinkID != links[0].ID || got[1].LinkID != links[1].ID {
		t.Fatalf("verdicts are not in the accounts' own order: %+v", got)
	}
	if got[0].Status != "mismatched" || len(got[0].Mismatches) != 1 {
		t.Errorf("the differing account says %+v, want mismatched with its one difference", got[0])
	}
	if got[1].Status != "matched" || len(got[1].Mismatches) != 0 {
		t.Errorf("the agreeing account says %+v, want matched with an empty list", got[1])
	}
	// Each verdict names whose it is, without the reader joining anything.
	if got[0].AccountID != links[0].AccountID || got[0].BrokerAccountName != links[0].BrokerAccountName {
		t.Errorf("the first verdict names account %s / %q, want %s / %q",
			got[0].AccountID, got[0].BrokerAccountName, links[0].AccountID, links[0].BrokerAccountName)
	}
	if got[1].AccountID != links[1].AccountID || got[1].BrokerAccountName != links[1].BrokerAccountName {
		t.Errorf("the second verdict names account %s / %q, want %s / %q",
			got[1].AccountID, got[1].BrokerAccountName, links[1].AccountID, links[1].BrokerAccountName)
	}
	if got[0].At == nil || got[1].At == nil {
		t.Fatalf("a checked account carries no time of its check: %+v", got)
	}
	// The agreeing account was checked LAST — which is exactly why a single
	// newest-first verdict for the connection reported agreement.
	if !got[1].At.After(*got[0].At) {
		t.Errorf("the fixture did not check the agreeing account last: %v then %v", got[0].At, got[1].At)
	}
}

func TestAnAccountNoRunEverCheckedSaysNotCheckedRatherThanNothing(t *testing.T) {
	api := newTestAPI(t)
	id, _ := api.createConnection(t)

	got := connectionReconciles(t, api, id)
	if len(got) != 1 {
		t.Fatalf("%d verdicts for a connection with one account: %+v", len(got), got)
	}
	// Present and saying not_checked, rather than absent: a missing entry and a
	// checked-and-agreed one look the same to anyone counting ticks.
	if got[0].Status != "not_checked" || got[0].At != nil || len(got[0].Mismatches) != 0 {
		t.Errorf("an unchecked account says %+v, want not_checked with no time and no differences", got[0])
	}

	var conn struct {
		LastSuccessfulSyncAt json.RawMessage `json:"last_successful_sync_at"`
	}
	_, body := do(t, api.owner, "GET", api.url+"/api/v1/tinvest/connections/"+id.String(), "")
	if err := json.Unmarshal([]byte(body), &conn); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if string(conn.LastSuccessfulSyncAt) != "null" {
		t.Errorf("last_successful_sync_at=%s, want an explicit null", conn.LastSuccessfulSyncAt)
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
