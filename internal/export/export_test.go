package export_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/creditcard"
	"babki.my/babki/internal/export"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/loan"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

type stack struct {
	url string
	c   *http.Client
	md  *marketdata.Store
	ca  *corporateaction.Store
}

func newStack(t *testing.T) stack {
	t.Helper()
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	instStore := instrument.NewStore(pool)
	opStore := operation.NewStore(pool)
	md := marketdata.NewStore(pool)
	ca := corporateaction.NewStore(pool)
	accStore := account.NewStore(pool)

	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	account.NewHandler(accStore, famStore, marketdata.NewConverter(md), nil, auth, sm).Mount(srv)
	instrument.NewHandler(instStore, auth, sm).Mount(srv)
	operation.NewHandler(operation.NewService(opStore), opStore, famStore, marketdata.NewConverter(md), auth, sm).Mount(srv)
	category.NewHandler(category.NewStore(pool), auth, sm).Mount(srv)
	loanSvc := loan.NewService(pool, accStore, operation.NewService(opStore), category.NewStore(pool))
	cardSvc := creditcard.NewService(pool, accStore, opStore)
	loan.NewHandler(loanSvc, auth, sm).Mount(srv)
	creditcard.NewHandler(cardSvc, auth, sm).Mount(srv)
	export.NewHandler(famStore, accStore, opStore, instStore, ca, md, category.NewStore(pool), loanSvc, cardSvc, auth, sm).Mount(srv)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	post(t, c, ts.URL+"/api/v1/setup", `{"space_name":"Семья","username":"alex","display_name":"Алекс","password":"secret123"}`, http.StatusCreated)
	return stack{url: ts.URL, c: c, md: md, ca: ca}
}

func post(t *testing.T, c *http.Client, url, body string, want int) map[string]any {
	t.Helper()
	resp, err := c.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("POST %s = %d, want %d: %s", url, resp.StatusCode, want, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func get(t *testing.T, c *http.Client, url string) *http.Response {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// The whole space in one document: members without their passwords, every
// account with its marks, its journal as stored — a move between accounts
// with the purchases it carried — and the taxes withheld it was told of, the
// papers named, the registry's events about them and the prices stated by
// hand, and nothing of another paper.
func TestTheExportHoldsTheWholeSpace(t *testing.T) {
	s := newStack(t)
	a := post(t, s.c, s.url+"/api/v1/accounts", `{"name":"Брокер","type":"brokerage","currency":"RUB"}`, http.StatusCreated)["id"].(string)
	b := post(t, s.c, s.url+"/api/v1/accounts", `{"name":"ИИС","type":"brokerage","currency":"RUB","institution":"Банк"}`, http.StatusCreated)["id"].(string)
	sber := post(t, s.c, s.url+"/api/v1/instruments", `{"type":"share","name":"Сбербанк","ticker":"SBER","isin":"RU0009029540","currency":"RUB"}`, http.StatusCreated)["id"].(string)
	post(t, s.c, s.url+"/api/v1/instruments", `{"type":"share","name":"Никто не купил","ticker":"NONE","isin":"RU000A0JX0J2","currency":"RUB"}`, http.StatusCreated)
	post(t, s.c, s.url+"/api/v1/operations", fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2026-07-01","quantity":"10","price":"100","currency":"RUB","note":"первая"}`, a, sber), http.StatusCreated)
	post(t, s.c, s.url+"/api/v1/operations/transfer", fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,"quantity":"4","occurred_on":"2026-07-02"}`, a, b, sber), http.StatusCreated)
	resp, err := s.c.Do(mustRequest(t, http.MethodPut, s.url+"/api/v1/accounts/"+b+"/balance", `{"as_of":"2026-07-03","amount_minor":12345}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("balance: %v %v", err, resp)
	}
	div := post(t, s.c, s.url+"/api/v1/operations", fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend","occurred_on":"2026-07-04","amount_minor":3000,"currency":"RUB"}`, a, sber), http.StatusCreated)["id"].(string)
	resp, err = s.c.Do(mustRequest(t, http.MethodPut, s.url+"/api/v1/operations/"+div+"/withheld-abroad", `{"tax_minor":300}`))
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("state the tax withheld: %v %v", err, resp)
	}
	id := uuid.MustParse(sber)
	if err := s.md.UpsertQuotes(t.Context(), []marketdata.Quote{
		{InstrumentID: id, On: day(t, "2026-07-05"), Price: decimal.RequireFromString("150"), Currency: "RUB", Source: "manual"},
		{InstrumentID: id, On: day(t, "2026-07-06"), Price: decimal.RequireFromString("151"), Currency: "RUB", Source: "moex"},
	}); err != nil {
		t.Fatal(err)
	}
	ratio := int64(2)
	if _, err := s.ca.Create(t.Context(), corporateaction.Event{
		Kind: corporateaction.KindSplit, ISIN: "RU0009029540", EffectiveOn: day(t, "2026-09-01"),
		RatioFrom: 1, RatioTo: ratio, Source: corporateaction.SourceManual, SourceRef: "https://example.org/split",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ca.Create(t.Context(), corporateaction.Event{
		Kind: corporateaction.KindSplit, ISIN: "RU000A0JX0J2", EffectiveOn: day(t, "2026-09-01"),
		RatioFrom: 1, RatioTo: 3, Source: corporateaction.SourceManual, SourceRef: "https://example.org/other",
	}); err != nil {
		t.Fatal(err)
	}

	resp = get(t, s.c, s.url+"/api/v1/export")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export = %d", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="babki-export-`) {
		t.Errorf("Content-Disposition = %q, want a download", cd)
	}
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "secret123") || strings.Contains(string(raw), "argon2") {
		t.Fatal("the export carries a password or its hash")
	}
	var doc struct {
		Format  string `json:"format"`
		Version int    `json:"version"`
		Space   struct {
			Name string `json:"name"`
		} `json:"space"`
		Members  []map[string]any `json:"members"`
		Accounts []struct {
			Name        string           `json:"name"`
			Institution string           `json:"institution"`
			Balances    []map[string]any `json:"balances"`
			Withheld    []map[string]any `json:"withheld_stated"`
			Operations  []struct {
				Type            string           `json:"type"`
				Note            string           `json:"note"`
				TransferGroupID *string          `json:"transfer_group_id"`
				Lots            []map[string]any `json:"lots"`
			} `json:"operations"`
		} `json:"accounts"`
		Instruments      []map[string]any `json:"instruments"`
		InstrumentEvents []map[string]any `json:"instrument_events"`
		ManualPrices     []map[string]any `json:"manual_prices"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Format != "babki.my/space-export" || doc.Version != 1 || doc.Space.Name != "Семья" {
		t.Errorf("header = %q v%d %q", doc.Format, doc.Version, doc.Space.Name)
	}
	if len(doc.Members) != 1 || doc.Members[0]["username"] != "alex" || doc.Members[0]["role"] != "owner" {
		t.Errorf("members = %v", doc.Members)
	}
	if len(doc.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(doc.Accounts))
	}
	byName := map[string]int{}
	for i, acc := range doc.Accounts {
		byName[acc.Name] = i
	}
	broker, iis := doc.Accounts[byName["Брокер"]], doc.Accounts[byName["ИИС"]]
	if len(broker.Operations) != 3 || broker.Operations[0].Type != "buy" || broker.Operations[0].Note != "первая" {
		t.Errorf("Брокер's journal = %+v", broker.Operations)
	}
	if len(broker.Withheld) != 1 || broker.Withheld[0]["tax_minor"] != float64(300) || broker.Withheld[0]["paid_on"] != "2026-07-04" {
		t.Errorf("Брокер's stated withholdings = %v, want the 300 stated on 2026-07-04", broker.Withheld)
	}
	if len(iis.Operations) != 1 || iis.Operations[0].Type != "transfer_in" || iis.Operations[0].TransferGroupID == nil ||
		len(iis.Operations[0].Lots) != 1 || iis.Operations[0].Lots[0]["cost_minor"] != float64(40_000) {
		t.Errorf("ИИС's journal = %+v, want the move with the purchase it carried", iis.Operations)
	}
	if iis.Institution != "Банк" || len(iis.Balances) != 1 || iis.Balances[0]["amount_minor"] != float64(12345) {
		t.Errorf("ИИС = %+v", iis)
	}
	if len(doc.Instruments) != 1 || doc.Instruments[0]["ticker"] != "SBER" {
		t.Errorf("instruments = %v, want SBER alone", doc.Instruments)
	}
	if len(doc.ManualPrices) != 1 || doc.ManualPrices[0]["price"] != "150" || doc.ManualPrices[0]["on"] != "2026-07-05" {
		t.Errorf("manual prices = %v, want the one stated by hand", doc.ManualPrices)
	}
	if len(doc.InstrumentEvents) != 1 || doc.InstrumentEvents[0]["kind"] != "split" {
		t.Errorf("events = %v, want SBER's split", doc.InstrumentEvents)
	}
}

// The family's categories leave with the journal, and so does how each row was
// filed and with whom.
func TestTheExportKeepsTheFamilysFiling(t *testing.T) {
	s := newStack(t)
	card := post(t, s.c, s.url+"/api/v1/accounts", `{"name":"Карта","type":"checking","currency":"RUB"}`, http.StatusCreated)["id"].(string)
	resp := get(t, s.c, s.url+"/api/v1/categories")
	var cats []struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cats); err != nil {
		t.Fatal(err)
	}
	var salary string
	for _, c := range cats {
		if c.Kind == "income" && c.Name == "Зарплата" {
			salary = c.ID
		}
	}
	post(t, s.c, s.url+"/api/v1/operations", fmt.Sprintf(`{"account_id":%q,"type":"deposit","occurred_on":"2026-09-05","amount_minor":18000000,"currency":"RUB","category_id":%q,"counterparty":"ООО Ромашка"}`, card, salary), http.StatusCreated)
	post(t, s.c, s.url+"/api/v1/category-rules", fmt.Sprintf(`{"category_id":%q,"field":"counterparty","pattern":"Ромашка"}`, salary), http.StatusCreated)

	raw, _ := io.ReadAll(get(t, s.c, s.url+"/api/v1/export").Body)
	var doc struct {
		Categories []struct {
			ID       string  `json:"id"`
			ParentID *string `json:"parent_id"`
		} `json:"categories"`
		Accounts []struct {
			Operations []struct {
				CategoryID   *string `json:"category_id"`
				Counterparty string  `json:"counterparty"`
			} `json:"operations"`
		} `json:"accounts"`
		Rules []struct {
			CategoryID string `json:"category_id"`
			Pattern    string `json:"pattern"`
		} `json:"category_rules"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Categories) != len(cats) {
		t.Errorf("categories = %d, want the family's %d", len(doc.Categories), len(cats))
	}
	seen := map[string]bool{}
	for _, c := range doc.Categories {
		if c.ParentID != nil && !seen[*c.ParentID] {
			t.Errorf("category %s comes before its parent", c.ID)
		}
		seen[c.ID] = true
	}
	if len(doc.Rules) != 1 || doc.Rules[0].CategoryID != salary || doc.Rules[0].Pattern != "Ромашка" {
		t.Errorf("rules = %+v", doc.Rules)
	}
	ops := doc.Accounts[0].Operations
	if len(ops) != 1 || ops[0].CategoryID == nil || *ops[0].CategoryID != salary || ops[0].Counterparty != "ООО Ромашка" {
		t.Errorf("the salary as exported: %+v", ops)
	}
}

// Everything the family entered leaves in one request, so only the owner may
// make it.
func TestOnlyTheOwnerExports(t *testing.T) {
	s := newStack(t)
	post(t, s.c, s.url+"/api/v1/members", `{"username":"vera","display_name":"Вера","password":"password9","role":"editor"}`, http.StatusCreated)
	jar, _ := cookiejar.New(nil)
	editor := &http.Client{Jar: jar}
	post(t, editor, s.url+"/api/v1/auth/login", `{"username":"vera","password":"password9"}`, http.StatusOK)
	if resp := get(t, editor, s.url+"/api/v1/export"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("an editor's export = %d, want 403", resp.StatusCode)
	}
	jar2, _ := cookiejar.New(nil)
	if resp := get(t, &http.Client{Jar: jar2}, s.url+"/api/v1/export"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an anonymous export = %d, want 401", resp.StatusCode)
	}
}

func mustRequest(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func day(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A loan's and a credit card's terms ride on their accounts in the export;
// every other account says null.
func TestTheExportCarriesLoanAndCardTerms(t *testing.T) {
	s := newStack(t)
	mk := func(name, typ string) string {
		return post(t, s.c, s.url+"/api/v1/accounts", fmt.Sprintf(`{"name":%q,"type":%q,"currency":"RUB"}`, name, typ), http.StatusCreated)["id"].(string)
	}
	mortgage, card, current := mk("Ипотека", "loan"), mk("Кредитка", "credit_card"), mk("Текущий", "checking")
	put := func(url, body string) {
		req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %s = %d", url, resp.StatusCode)
		}
	}
	put(s.url+"/api/v1/accounts/"+mortgage+"/loan", `{"principal_minor":300000000,"annual_rate":"18.5","term_months":240,"issued_on":"2026-03-15","kind":"annuity"}`)
	put(s.url+"/api/v1/accounts/"+card+"/credit-card", `{"limit_minor":15000000,"statement_day":1,"payment_days":20,"grace_kind":"long","grace_days":120,"min_percent":"3","min_floor_minor":30000,"annual_rate":"39.9","own_rate":"15"}`)

	var doc struct {
		Accounts []struct {
			ID         string          `json:"id"`
			Loan       json.RawMessage `json:"loan"`
			CreditCard json.RawMessage `json:"credit_card"`
		} `json:"accounts"`
	}
	resp := get(t, s.c, s.url+"/api/v1/export")
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, a := range doc.Accounts {
		switch a.ID {
		case mortgage:
			seen++
			if !strings.Contains(string(a.Loan), `"term_months":240`) || string(a.CreditCard) != "null" {
				t.Errorf("mortgage: loan %s, card %s", a.Loan, a.CreditCard)
			}
		case card:
			seen++
			if !strings.Contains(string(a.CreditCard), `"grace_days":120`) || !strings.Contains(string(a.CreditCard), `"own_rate":"15"`) || string(a.Loan) != "null" {
				t.Errorf("card: card %s, loan %s", a.CreditCard, a.Loan)
			}
		case current:
			seen++
			if string(a.Loan) != "null" || string(a.CreditCard) != "null" {
				t.Errorf("current: loan %s, card %s", a.Loan, a.CreditCard)
			}
		}
	}
	if seen != 3 {
		t.Errorf("saw %d of the 3 accounts", seen)
	}
}
