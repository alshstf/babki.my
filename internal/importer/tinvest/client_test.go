package tinvest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// This file is package tinvest (not tinvest_test) so it can reach the
// unexported Client.sleep field to control the 429 backoff in tests without
// a real wait — see the field's own doc comment.

// readFixture returns one testdata file's contents.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// route is one path's canned response.
type route struct {
	status int
	body   []byte
	header http.Header
}

// serve starts an httptest.Server that dispatches by exact URL path to
// routes, and records the raw request body seen for each path (last one
// wins, matching internal/marketdata/moex/moex_test.go's serve helper).
func serve(t *testing.T, routes map[string]route) (*httptest.Server, map[string][]byte) {
	t.Helper()
	gotBodies := make(map[string][]byte)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt, ok := routes[r.URL.Path]
		if !ok {
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, err := readRequestBody(r)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		gotBodies[r.URL.Path] = body
		for k, vs := range rt.header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rt.status)
		_, _ = w.Write(rt.body)
	}))
	t.Cleanup(srv.Close)
	return srv, gotBodies
}

// readRequestBody reads and closes r.Body.
func readRequestBody(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}

const opsPath = "/tinkoff.public.invest.api.contract.v1.OperationsService/GetOperationsByCursor"

// -------------------------------------------------------------------------
// GetAccounts
// -------------------------------------------------------------------------

func TestGetAccounts_ParsesFixture(t *testing.T) {
	srv, gotBodies := serve(t, map[string]route{
		"/tinkoff.public.invest.api.contract.v1.UsersService/GetAccounts": {
			status: http.StatusOK,
			body:   readFixture(t, "accounts.json"),
		},
	})
	c := NewClient(srv.Client(), srv.URL, "test-token", nil)

	accounts, err := c.GetAccounts(context.Background())
	if err != nil {
		t.Fatalf("GetAccounts: %v", err)
	}
	if len(accounts) != 3 {
		t.Fatalf("len(accounts) = %d, want 3: %+v", len(accounts), accounts)
	}

	first := accounts[0]
	if first.ID != "2000000001" || first.Name != "Брокерский счет" ||
		first.Type != "ACCOUNT_TYPE_TINKOFF" || first.Status != "ACCOUNT_STATUS_OPEN" {
		t.Fatalf("accounts[0] = %+v, unexpected", first)
	}
	if first.OpenedOn == nil {
		t.Fatalf("accounts[0].OpenedOn = nil, want non-nil")
	}
	wantOpened := time.Date(2021, 3, 15, 0, 0, 0, 0, time.UTC)
	if !first.OpenedOn.Equal(wantOpened) {
		t.Errorf("accounts[0].OpenedOn = %s, want %s", first.OpenedOn, wantOpened)
	}

	// Third account has no openedDate field at all in the fixture: OpenedOn
	// must come back nil, not the zero time.Time — a caller checking "do we
	// know when this was opened" must be able to tell "unknown" apart from
	// "opened at 0001-01-01".
	if got := accounts[2]; got.OpenedOn != nil {
		t.Errorf("accounts[2].OpenedOn = %v, want nil (fixture has no openedDate)", got.OpenedOn)
	}

	// Request body: GetAccounts filters by nothing, so the body is "{}".
	body, ok := gotBodies["/tinkoff.public.invest.api.contract.v1.UsersService/GetAccounts"]
	if !ok {
		t.Fatalf("no request recorded for GetAccounts")
	}
	if strings.TrimSpace(string(body)) != "{}" {
		t.Errorf("GetAccounts request body = %s, want {}", body)
	}
}

func TestGetAccounts_SetsAuthAndContentType(t *testing.T) {
	var gotAuth, gotContentType, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"accounts":[]}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.Client(), srv.URL, "my-secret-token", nil)
	if _, err := c.GetAccounts(context.Background()); err != nil {
		t.Fatalf("GetAccounts: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/tinkoff.public.invest.api.contract.v1.UsersService/GetAccounts" {
		t.Errorf("path = %q, want the UsersService/GetAccounts path", gotPath)
	}
	if gotAuth != "Bearer my-secret-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer my-secret-token")
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
}

// -------------------------------------------------------------------------
// OperationsAll: cursor pagination
// -------------------------------------------------------------------------

func TestOperationsAll_WalksTwoPages(t *testing.T) {
	page1 := readFixture(t, "operations_page1.json")
	page2 := readFixture(t, "operations_page2.json")

	var requests []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != opsPath {
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var reqBody map[string]any
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requests = append(requests, reqBody)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if len(requests) == 1 {
			_, _ = w.Write(page1)
		} else {
			_, _ = w.Write(page2)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.Client(), srv.URL, "tok", nil)
	items, err := c.OperationsAll(context.Background(), "2000000001", time.Time{})
	if err != nil {
		t.Fatalf("OperationsAll: %v", err)
	}

	if len(requests) != 2 {
		t.Fatalf("made %d requests, want exactly 2 (one per page)", len(requests))
	}
	if cursor, _ := requests[0]["cursor"].(string); cursor != "" {
		t.Errorf("first request cursor = %q, want empty", cursor)
	}
	if cursor, _ := requests[1]["cursor"].(string); cursor != "cursor-2" {
		t.Errorf("second request cursor = %q, want %q", cursor, "cursor-2")
	}
	for i, req := range requests {
		limit, ok := req["limit"].(float64)
		if !ok || limit != 1000 {
			t.Errorf("request %d limit = %v, want 1000", i, req["limit"])
		}
		if _, present := req["from"]; present {
			t.Errorf("request %d has a \"from\" key, want it omitted (from was the zero time.Time)", i)
		}
		if _, present := req["state"]; present {
			t.Errorf("request %d filters by state, want it never sent", i)
		}
	}

	// 2 items on page 1 + 1 on page 2.
	if len(items) != 3 {
		t.Fatalf("len(items) = %d, want 3: %+v", len(items), items)
	}

	op1 := items[0]
	if op1.ID != "op-1" || op1.Type != "OPERATION_TYPE_BUY" || op1.State != "OPERATION_STATE_EXECUTED" {
		t.Fatalf("items[0] = %+v, unexpected", op1)
	}
	wantDate := time.Date(2026, 1, 10, 10, 0, 0, 0, time.UTC)
	if !op1.Date.Equal(wantDate) {
		t.Errorf("items[0].Date = %s, want %s", op1.Date, wantDate)
	}
	if op1.Quantity != 100 {
		t.Errorf("items[0].Quantity = %d, want 100", op1.Quantity)
	}
	wantPayment := decimal.RequireFromString("-15230")
	if !op1.Payment.Decimal().Equal(wantPayment) {
		t.Errorf("items[0].Payment.Decimal() = %s, want %s", op1.Payment.Decimal(), wantPayment)
	}
	if op1.Payment.Currency != "RUB" {
		t.Errorf("items[0].Payment.Currency = %q, want RUB (lowercase \"rub\" in the fixture must be upper-cased)", op1.Payment.Currency)
	}
	wantCommission := decimal.RequireFromString("-5.5")
	if !op1.Commission.Decimal().Equal(wantCommission) {
		t.Errorf("items[0].Commission.Decimal() = %s, want %s", op1.Commission.Decimal(), wantCommission)
	}

	// Raw must be exactly the item's own bytes, so re-decoding it must
	// reproduce the same id.
	var rawDecoded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(op1.Raw, &rawDecoded); err != nil {
		t.Fatalf("decode items[0].Raw: %v", err)
	}
	if rawDecoded.ID != "op-1" {
		t.Errorf("items[0].Raw decodes to id %q, want op-1", rawDecoded.ID)
	}

	// The third operation (page 2) is OPERATION_STATE_CANCELED: it must be
	// present in the result, proving state is never filtered — the mirror
	// needs canceled operations too.
	op3 := items[2]
	if op3.ID != "op-3" || op3.State != "OPERATION_STATE_CANCELED" {
		t.Fatalf("items[2] = %+v, want the canceled dividend operation (state must not be filtered)", op3)
	}
}

func TestOperationsAll_EmptyHistoryReturnsImmediately(t *testing.T) {
	var requestCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"hasNext":false,"nextCursor":"","items":[]}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.Client(), srv.URL, "tok", nil)
	items, err := c.OperationsAll(context.Background(), "acc", time.Time{})
	if err != nil {
		t.Fatalf("OperationsAll: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("len(items) = %d, want 0", len(items))
	}
	if requestCount != 1 {
		t.Errorf("made %d requests, want exactly 1", requestCount)
	}
}

func TestOperationsAll_SendsFromWhenSet(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"hasNext":false,"nextCursor":"","items":[]}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.Client(), srv.URL, "tok", nil)
	from := time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := c.OperationsAll(context.Background(), "acc", from); err != nil {
		t.Fatalf("OperationsAll: %v", err)
	}

	got, ok := gotBody["from"].(string)
	if !ok {
		t.Fatalf("request has no \"from\" string field: %+v", gotBody)
	}
	want := "2020-06-01T00:00:00Z"
	if !strings.HasPrefix(got, want) {
		t.Errorf("from = %q, want prefix %q", got, want)
	}
}

// TestOperationsAll_StuckCursorErrorsRatherThanLoopingForever reproduces a
// gateway bug this client must survive without hanging: hasNext=true but
// next_cursor never actually moves past what was requested. The fake
// server also caps itself at a handful of requests and then answers
// hasNext=false regardless, so that if the guard this test exists to check
// were ever removed by mistake, this test fails fast (empty result, no
// error) instead of hanging until the whole test binary's deadline.
func TestOperationsAll_StuckCursorErrorsRatherThanLoopingForever(t *testing.T) {
	const forcedStopAfter = 5
	var requestCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if requestCount >= forcedStopAfter {
			_, _ = w.Write([]byte(`{"hasNext":false,"nextCursor":"","items":[]}`))
			return
		}
		// hasNext claims more data, but next_cursor is identical to what a
		// first request always carries ("") — the cursor never advances.
		_, _ = w.Write([]byte(`{"hasNext":true,"nextCursor":"","items":[]}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.Client(), srv.URL, "tok", nil)
	items, err := c.OperationsAll(context.Background(), "acc", time.Time{})
	if err == nil {
		t.Fatalf("OperationsAll returned no error (items=%+v), want an error about the cursor not advancing", items)
	}
	if !strings.Contains(err.Error(), "cursor did not advance") {
		t.Errorf("error = %q, want it to mention the cursor not advancing", err)
	}
	if requestCount != 1 {
		t.Errorf("made %d requests, want exactly 1 (the guard must catch this on the very first response)", requestCount)
	}
}

// -------------------------------------------------------------------------
// GetPortfolio / GetPositions
// -------------------------------------------------------------------------

func TestGetPortfolio_ParsesFixture(t *testing.T) {
	srv, _ := serve(t, map[string]route{
		"/tinkoff.public.invest.api.contract.v1.OperationsService/GetPortfolio": {
			status: http.StatusOK,
			body:   readFixture(t, "portfolio.json"),
		},
	})
	c := NewClient(srv.Client(), srv.URL, "tok", nil)

	positions, err := c.GetPortfolio(context.Background(), "2000000001")
	if err != nil {
		t.Fatalf("GetPortfolio: %v", err)
	}
	if len(positions) != 2 {
		t.Fatalf("len(positions) = %d, want 2: %+v", len(positions), positions)
	}

	share := positions[0]
	if share.FIGI != "BBG004730N88" || share.InstrumentUID != "uid-sber" || share.InstrumentType != "share" {
		t.Fatalf("positions[0] = %+v, unexpected", share)
	}
	if share.Blocked {
		t.Errorf("positions[0].Blocked = true, want false")
	}
	wantQty := decimal.RequireFromString("100")
	if !share.Quantity.Decimal().Equal(wantQty) {
		t.Errorf("positions[0].Quantity.Decimal() = %s, want %s", share.Quantity.Decimal(), wantQty)
	}

	bond := positions[1]
	if !bond.Blocked {
		t.Errorf("positions[1].Blocked = false, want true")
	}
	wantBondQty := decimal.RequireFromString("10.5")
	if !bond.Quantity.Decimal().Equal(wantBondQty) {
		t.Errorf("positions[1].Quantity.Decimal() = %s, want %s", bond.Quantity.Decimal(), wantBondQty)
	}
}

func TestGetPositions_MergesMoneyAndBlockedByCurrency(t *testing.T) {
	srv, _ := serve(t, map[string]route{
		"/tinkoff.public.invest.api.contract.v1.OperationsService/GetPositions": {
			status: http.StatusOK,
			body:   readFixture(t, "positions.json"),
		},
	})
	c := NewClient(srv.Client(), srv.URL, "tok", nil)

	balances, err := c.GetPositions(context.Background(), "2000000001")
	if err != nil {
		t.Fatalf("GetPositions: %v", err)
	}
	if len(balances) != 2 {
		t.Fatalf("len(balances) = %d, want 2: %+v", len(balances), balances)
	}

	// Sorted ascending: RUB before USD.
	rub, usd := balances[0], balances[1]
	if rub.Currency != "RUB" {
		t.Fatalf("balances[0].Currency = %q, want RUB", rub.Currency)
	}
	if usd.Currency != "USD" {
		t.Fatalf("balances[1].Currency = %q, want USD", usd.Currency)
	}

	wantRUBValue := decimal.RequireFromString("10500.25")
	if !rub.Value.Equal(wantRUBValue) {
		t.Errorf("RUB.Value = %s, want %s", rub.Value, wantRUBValue)
	}
	wantRUBBlocked := decimal.RequireFromString("100")
	if !rub.Blocked.Equal(wantRUBBlocked) {
		t.Errorf("RUB.Blocked = %s, want %s", rub.Blocked, wantRUBBlocked)
	}

	// USD appears only in "money", not "blocked": Blocked must default to
	// zero rather than being treated as an error or left as decimal's own
	// zero-value struct with a different internal representation of zero.
	wantUSDValue := decimal.RequireFromString("42")
	if !usd.Value.Equal(wantUSDValue) {
		t.Errorf("USD.Value = %s, want %s", usd.Value, wantUSDValue)
	}
	if !usd.Blocked.Equal(decimal.Zero) {
		t.Errorf("USD.Blocked = %s, want 0", usd.Blocked)
	}
}

// -------------------------------------------------------------------------
// InstrumentByUID
// -------------------------------------------------------------------------

func TestInstrumentByUID_ParsesFixture(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(readFixture(t, "instrument.json"))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.Client(), srv.URL, "tok", nil)
	got, err := c.InstrumentByUID(context.Background(), "uid-sber")
	if err != nil {
		t.Fatalf("InstrumentByUID: %v", err)
	}

	if gotBody["idType"] != "INSTRUMENT_ID_TYPE_UID" {
		t.Errorf("request idType = %v, want INSTRUMENT_ID_TYPE_UID", gotBody["idType"])
	}
	if gotBody["id"] != "uid-sber" {
		t.Errorf("request id = %v, want uid-sber", gotBody["id"])
	}

	want := InstrumentBrief{
		UID:            "uid-sber",
		FIGI:           "BBG004730N88",
		ISIN:           "RU0009029540",
		Ticker:         "SBER",
		Name:           "Сбер Банк",
		Currency:       "RUB",
		InstrumentType: "share",
	}
	if got != want {
		t.Errorf("InstrumentByUID = %+v, want %+v", got, want)
	}
}

// TestInstrumentByUID_NominalDecodesWhenPresent proves the defensive
// nominal/blocked decoding actually works, even though (per wireInstrument's
// doc comment) the real gateway's GetInstrumentBy response never carries
// these fields today: if it ever did, this client must not silently drop
// them.
func TestInstrumentByUID_NominalDecodesWhenPresent(t *testing.T) {
	body := `{"instrument":{"uid":"uid-bond","figi":"F1","isin":"I1","ticker":"BOND1",
		"name":"Bond","currency":"rub","instrumentType":"bond",
		"nominal":{"currency":"rub","units":"1000","nano":0},"blocked":true}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.Client(), srv.URL, "tok", nil)
	got, err := c.InstrumentByUID(context.Background(), "uid-bond")
	if err != nil {
		t.Fatalf("InstrumentByUID: %v", err)
	}
	if !got.Blocked {
		t.Errorf("Blocked = false, want true")
	}
	wantNominal := decimal.RequireFromString("1000")
	if !got.Nominal.Decimal().Equal(wantNominal) {
		t.Errorf("Nominal.Decimal() = %s, want %s", got.Nominal.Decimal(), wantNominal)
	}
	if got.Nominal.Currency != "RUB" {
		t.Errorf("Nominal.Currency = %q, want RUB", got.Nominal.Currency)
	}
}

// -------------------------------------------------------------------------
// Errors: 401/40003, generic status, request encode failures
// -------------------------------------------------------------------------

func TestGetAccounts_401ReturnsErrTokenInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":16,"message":"authentication token is missing or invalid","description":40003}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.Client(), srv.URL, "stale-token", nil)
	_, err := c.GetAccounts(context.Background())
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("GetAccounts error = %v, want ErrTokenInvalid", err)
	}
}

func TestGetAccounts_401WithEmptyBodyStillReturnsErrTokenInvalid(t *testing.T) {
	// The documented pairing is 401 + description 40003, but the brief's own
	// rule is "40003/401" — either signal alone must be enough. An empty (or
	// otherwise unparsable) body must not defeat the plain HTTP status check.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.Client(), srv.URL, "tok", nil)
	_, err := c.GetAccounts(context.Background())
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("GetAccounts error = %v, want ErrTokenInvalid", err)
	}
}

func TestGetAccounts_NonAuthStatusWithDescription40003ReturnsErrTokenInvalid(t *testing.T) {
	// Defensive branch: description 40003 arriving under some status other
	// than 401 must still be recognized.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":16,"message":"authentication token is missing or invalid","description":40003}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.Client(), srv.URL, "tok", nil)
	_, err := c.GetAccounts(context.Background())
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("GetAccounts error = %v, want ErrTokenInvalid", err)
	}
}

func TestGetAccounts_GenericErrorStatusIsNotErrTokenInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.Client(), srv.URL, "tok", nil)
	_, err := c.GetAccounts(context.Background())
	if err == nil {
		t.Fatalf("GetAccounts returned no error for a 500 response")
	}
	if errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("GetAccounts error = %v, want anything but ErrTokenInvalid for a plain 500", err)
	}
	if !strings.Contains(err.Error(), "tinvest:") {
		t.Errorf("error = %q, want it prefixed with the package name", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, want it to mention the status code", err)
	}
}

// -------------------------------------------------------------------------
// 429 rate limiting
// -------------------------------------------------------------------------

func TestDo_429ThenSuccessWaitsAndRetriesOnce(t *testing.T) {
	var requestCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if requestCount == 1 {
			w.Header().Set(rateLimitResetHeader, "3")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"accounts":[]}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.Client(), srv.URL, "tok", nil)
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}

	if _, err := c.GetAccounts(context.Background()); err != nil {
		t.Fatalf("GetAccounts: %v", err)
	}
	if requestCount != 2 {
		t.Fatalf("made %d requests, want exactly 2 (one 429, one retry)", requestCount)
	}
	wantSlept := []time.Duration{3 * time.Second}
	if len(slept) != len(wantSlept) || slept[0] != wantSlept[0] {
		t.Fatalf("slept = %v, want %v", slept, wantSlept)
	}
}

func TestDo_429TwiceReturnsErrorAfterExactlyOneRetry(t *testing.T) {
	var requestCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set(rateLimitResetHeader, "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.Client(), srv.URL, "tok", nil)
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}

	_, err := c.GetAccounts(context.Background())
	if err == nil {
		t.Fatalf("GetAccounts returned no error, want a rate-limit error after the retry also gets 429")
	}
	if requestCount != 2 {
		t.Fatalf("made %d requests, want exactly 2 (no unbounded retry loop)", requestCount)
	}
	if len(slept) != 1 {
		t.Fatalf("slept %d times, want exactly 1", len(slept))
	}
	if !strings.Contains(err.Error(), "rate limited again") {
		t.Errorf("error = %q, want it to say the retry was also rate limited", err)
	}
}

func TestDo_429SleepErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(rateLimitResetHeader, "3")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.Client(), srv.URL, "tok", nil)
	sleepErr := context.Canceled
	c.sleep = func(_ context.Context, _ time.Duration) error {
		return sleepErr
	}

	_, err := c.GetAccounts(context.Background())
	if err == nil {
		t.Fatalf("GetAccounts returned no error, want the sleep error surfaced")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
}

func TestParseRateLimitReset(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"typical value", "3", 3 * time.Second},
		{"at the cap", "65", 65 * time.Second},
		{"just under the cap", "64", 64 * time.Second},
		{"one second", "1", 1 * time.Second},
		{"missing header defaults to the cap", "", 65 * time.Second},
		{"unparsable defaults to the cap", "soon", 65 * time.Second},
		{"zero defaults to the cap", "0", 65 * time.Second},
		{"negative defaults to the cap", "-5", 65 * time.Second},
		{"excessive value is capped", "100000", 65 * time.Second},
		{"just over the cap", "66", 65 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRateLimitReset(tc.raw)
			if got != tc.want {
				t.Errorf("parseRateLimitReset(%q) = %s, want %s", tc.raw, got, tc.want)
			}
		})
	}
}

func TestCtxSleep_RespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := ctxSleep(ctx, 5*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("ctxSleep returned no error for an already-canceled context")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("ctxSleep took %s for a canceled context, want it to return almost immediately", elapsed)
	}
}

func TestCtxSleep_WaitsOutTheDurationWhenNotCanceled(t *testing.T) {
	const wait = 30 * time.Millisecond
	start := time.Now()
	err := ctxSleep(context.Background(), wait)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("ctxSleep: %v", err)
	}
	if elapsed < wait {
		t.Fatalf("ctxSleep returned after %s, want at least %s (it must actually wait, not just check ctx.Done)", elapsed, wait)
	}
}

// -------------------------------------------------------------------------
// MoneyValue.Decimal / Quotation.Decimal edge cases
// -------------------------------------------------------------------------

func TestMoneyValue_Decimal(t *testing.T) {
	cases := []struct {
		name       string
		units      int64
		nano       int32
		wantString string
	}{
		{"negative zero: zero units, negative nano", 0, -200_000_000, "-0.2"},
		{"negative units and negative nano agree in sign", -200, -200_000_000, "-200.2"},
		{"sub-cent precision", 1, 5, "1.000000005"},
		{"positive whole number", 100, 0, "100"},
		{"positive units and positive nano", 152, 300_000_000, "152.3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := MoneyValue{Currency: "RUB", Units: tc.units, Nano: tc.nano}
			want := decimal.RequireFromString(tc.wantString)
			if got := m.Decimal(); !got.Equal(want) {
				t.Errorf("MoneyValue{Units:%d,Nano:%d}.Decimal() = %s, want %s", tc.units, tc.nano, got, want)
			}
		})
	}
}

func TestQuotation_Decimal(t *testing.T) {
	cases := []struct {
		name       string
		units      int64
		nano       int32
		wantString string
	}{
		{"negative zero", 0, -200_000_000, "-0.2"},
		{"negative units and nano", -200, -200_000_000, "-200.2"},
		{"sub-cent precision", 1, 5, "1.000000005"},
		{"whole lots", 10, 0, "10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := Quotation{Units: tc.units, Nano: tc.nano}
			want := decimal.RequireFromString(tc.wantString)
			if got := q.Decimal(); !got.Equal(want) {
				t.Errorf("Quotation{Units:%d,Nano:%d}.Decimal() = %s, want %s", tc.units, tc.nano, got, want)
			}
		})
	}
}

// -------------------------------------------------------------------------
// NewClient / NewHTTPClient
// -------------------------------------------------------------------------

func TestNewClient_Defaults(t *testing.T) {
	c := NewClient(nil, "", "tok", nil)

	if c.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q, want DefaultBaseURL (%q)", c.baseURL, DefaultBaseURL)
	}
	if c.log == nil {
		t.Errorf("log = nil, want slog.Default()")
	}
	if c.http == nil {
		t.Fatalf("http client = nil")
	}
	if c.http.Timeout != 30*time.Second {
		t.Errorf("http client Timeout = %s, want 30s", c.http.Timeout)
	}
	if c.http.Timeout <= 0 {
		t.Errorf("http client Timeout = %s, want a positive bound (0 means unbounded)", c.http.Timeout)
	}
	if c.sleep == nil {
		t.Errorf("sleep = nil, want ctxSleep")
	}
}

func TestNewClient_ExplicitBaseURLOverridesDefault(t *testing.T) {
	c := NewClient(&http.Client{}, "https://example.test/rest", "tok", nil)
	if c.baseURL != "https://example.test/rest" {
		t.Errorf("baseURL = %q, want the explicit one", c.baseURL)
	}
}

func TestNewHTTPClient_TimeoutIsSetAsGiven(t *testing.T) {
	hc, err := NewHTTPClient(7 * time.Second)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	if hc.Timeout != 7*time.Second {
		t.Errorf("Timeout = %s, want 7s", hc.Timeout)
	}
}

// systemPoolBaseline reproduces NewHTTPClient's own fallback (system pool,
// or a fresh empty one if unavailable) so the "how many certs does the
// system contribute" baseline is measured the same way in the test as it
// is inside the function under test.
func systemPoolBaseline(t *testing.T) *x509.CertPool {
	t.Helper()
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	return pool
}

func TestNewHTTPClient_PoolHasExactlyOneCertMoreThanSystem_AndItIsOurs(t *testing.T) {
	baseline := systemPoolBaseline(t)
	//nolint:staticcheck // Subjects() is deprecated but is the only stdlib
	// way to count and identify individual certs in a CertPool; the count
	// difference is measured against a baseline pool built the same way (not
	// assumed to be some fixed number), so it holds regardless of whether a
	// given platform's SystemCertPool populates Subjects() with real system
	// entries (Linux does) or leaves it empty and delegates to the OS
	// verifier (observed on this darwin dev machine) — either way, our own
	// AppendCertsFromPEM call always adds exactly one Subjects() entry.
	baseCount := len(baseline.Subjects())

	hc, err := NewHTTPClient(time.Second)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	transport, ok := hc.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs == nil {
		t.Fatalf("Transport = %#v, want an *http.Transport with TLSClientConfig.RootCAs set", hc.Transport)
	}
	gotPool := transport.TLSClientConfig.RootCAs
	//nolint:staticcheck // see baseCount above
	gotSubjects := gotPool.Subjects()

	if len(gotSubjects) != baseCount+1 {
		t.Fatalf("pool has %d subjects, want exactly %d (system baseline %d + our one embedded root)",
			len(gotSubjects), baseCount+1, baseCount)
	}

	ourCert := parseEmbeddedCert(t)
	found := false
	for _, s := range gotSubjects {
		if bytes.Equal(s, ourCert.RawSubject) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("the extra certificate in the pool does not match the embedded Russian Trusted Root CA's subject")
	}
}

// parseEmbeddedCert parses the embedded PEM once, for tests that need to
// inspect the certificate itself.
func parseEmbeddedCert(t *testing.T) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(russianTrustedRootCAPEM)
	if block == nil {
		t.Fatalf("russian_trusted_root_ca.pem: no PEM block found")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse embedded certificate: %v", err)
	}
	return cert
}

func TestEmbeddedCert_ParsesAndIsNotExpired(t *testing.T) {
	cert := parseEmbeddedCert(t)
	if !cert.NotAfter.After(time.Now()) {
		t.Fatalf("embedded certificate expired on %s (now: %s) — the Russian Trusted Root CA needs re-pinning",
			cert.NotAfter, time.Now())
	}
	if !cert.IsCA {
		t.Errorf("embedded certificate is not a CA certificate")
	}
	// Self-signed: issuer and subject must be identical, and the signature
	// must verify against the certificate's own public key.
	if err := cert.CheckSignatureFrom(cert); err != nil {
		t.Errorf("embedded certificate does not self-verify: %v", err)
	}
}

func TestEmbeddedCert_FingerprintMatchesWhatWasVerifiedOutOfBand(t *testing.T) {
	cert := parseEmbeddedCert(t)
	sum := sha256.Sum256(cert.Raw)

	hexParts := make([]string, len(sum))
	for i, b := range sum {
		hexParts[i] = fmt.Sprintf("%02X", b)
	}
	got := strings.Join(hexParts, ":")

	// Literal, independently re-derivable value (not the russianTrustedRootCAFingerprint
	// constant this same file's other code also declares): reproduced with
	// `openssl x509 -in russian_trusted_root_ca.pem -noout -fingerprint -sha256`
	// against the file actually embedded in this package, 2026-08-04.
	const want = "D2:6D:2D:02:31:B7:C3:9F:92:CC:73:85:12:BA:54:10:35:19:E4:40:5D:68:B5:BD:70:3E:97:88:CA:8E:CF:31"
	if got != want {
		t.Fatalf("embedded certificate fingerprint = %s, want %s", got, want)
	}
}

func TestParseWireInt64(t *testing.T) {
	cases := []struct {
		raw     string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"100", 100, false},
		{"-15230", -15230, false},
		{"9223372036854775807", 9223372036854775807, false},
		{"not-a-number", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseWireInt64(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseWireInt64(%q) returned no error, want one", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseWireInt64(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("parseWireInt64(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

func TestWireMoneyValue_CurrencyIsUppercased(t *testing.T) {
	w := wireMoneyValue{Currency: "rub", Units: "10", Nano: 0}
	got, err := w.parse()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Currency != "RUB" {
		t.Errorf("Currency = %q, want RUB", got.Currency)
	}
}
