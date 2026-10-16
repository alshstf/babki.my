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
	"babki.my/babki/internal/allocation"
	"babki.my/babki/internal/budget"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/corporateaction"
	"babki.my/babki/internal/creditcard"
	"babki.my/babki/internal/export"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/goals"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/loan"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/receipt"
	"babki.my/babki/internal/recurring"
	"babki.my/babki/internal/taxyear"
	"babki.my/babki/internal/valuables"
)

type stack struct {
	url    string
	c      *http.Client
	goals  *goals.Service
	hidden *recurring.Hidden
	md     *marketdata.Store
	ca     *corporateaction.Store
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
	cardSvc := creditcard.NewService(pool, accStore, opStore, category.NewStore(pool))
	loan.NewHandler(loanSvc, auth, sm).Mount(srv)
	creditcard.NewHandler(cardSvc, auth, sm).Mount(srv)
	taxSvc := taxyear.NewService(accStore, nil, famStore, md).WithEarnings(opStore, category.NewStore(pool)).WithEntries(pool, opStore)
	allocSvc, goalsSvc, hidden := allocation.NewService(pool, nil), goals.NewService(pool, nil, accStore), recurring.NewHidden(pool)
	allocation.NewHandler(allocSvc, auth, sm).Mount(srv)
	export.NewHandler(famStore, accStore, opStore, instStore, ca, md, category.NewStore(pool), loanSvc, cardSvc,
		budget.NewService(pool, nil, category.NewStore(pool)), receipt.NewService(pool, opStore, accStore, category.NewStore(pool), operation.NewService(opStore)),
		allocSvc, auth, sm).WithValuables(valuables.NewService(pool, accStore, md)).WithWages(taxSvc).
		WithPlanning(instStore, goalsSvc, allocSvc).WithHidden(hidden).Mount(srv)
	valuables.NewHandler(valuables.NewService(pool, accStore, md), auth, sm).Mount(srv)
	taxyear.NewHandler(taxSvc, auth, sm).Mount(srv)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	post(t, c, ts.URL+"/api/v1/setup", `{"space_name":"Семья","username":"alex","display_name":"Алекс","password":"secret123"}`, http.StatusCreated)
	return stack{url: ts.URL, c: c, goals: goalsSvc, hidden: hidden, md: md, ca: ca}
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

// The family's categories leave with the journal, each with its mark for tax,
// and so does how each row was filed and with whom, and a wage's payslip.
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
	pay := post(t, s.c, s.url+"/api/v1/operations", fmt.Sprintf(`{"account_id":%q,"type":"deposit","occurred_on":"2026-09-05","amount_minor":18000000,"currency":"RUB","category_id":%q,"counterparty":"ООО Ромашка"}`, card, salary), http.StatusCreated)["id"].(string)
	req, _ := http.NewRequest(http.MethodPut, s.url+"/api/v1/tax-year/wages/"+pay, strings.NewReader(`{"gross_minor":20689700,"withheld_minor":2689700}`))
	req.Header.Set("Content-Type", "application/json")
	if r, err := s.c.Do(req); err != nil || r.StatusCode != http.StatusNoContent {
		t.Fatalf("the payslip = %v %v", r, err)
	}
	post(t, s.c, s.url+"/api/v1/category-rules", fmt.Sprintf(`{"category_id":%q,"field":"counterparty","pattern":"Ромашка"}`, salary), http.StatusCreated)

	raw, _ := io.ReadAll(get(t, s.c, s.url+"/api/v1/export").Body)
	var doc struct {
		Categories []struct {
			ID       string  `json:"id"`
			ParentID *string `json:"parent_id"`
			TaxMark  *string `json:"tax_mark"`
		} `json:"categories"`
		Wages []struct {
			OperationID string `json:"operation_id"`
			Gross       int64  `json:"gross_minor"`
			Withheld    int64  `json:"withheld_minor"`
		} `json:"wage_entries"`
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
		if c.ID == salary && (c.TaxMark == nil || *c.TaxMark != "wages") {
			t.Errorf("the salary's tax mark = %v", c.TaxMark)
		}
	}
	if len(doc.Wages) != 1 || doc.Wages[0].OperationID != pay || doc.Wages[0].Gross != 20_689_700 || doc.Wages[0].Withheld != 2_689_700 {
		t.Errorf("the payslips = %+v", doc.Wages)
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
	put(s.url+"/api/v1/accounts/"+card+"/credit-card/bank", `{"stated_on":"2026-01-05","grace":{"on":"2026-01-20","amount_minor":1234500},"minimum":null}`)

	var doc struct {
		Accounts []struct {
			ID         string          `json:"id"`
			Loan       json.RawMessage `json:"loan"`
			CreditCard json.RawMessage `json:"credit_card"`
			Bank       json.RawMessage `json:"credit_card_bank_figures"`
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
			if string(a.Bank) != `{"grace":{"amount_minor":1234500,"on":"2026-01-20"},"minimum":null,"stated_on":"2026-01-05"}` {
				t.Errorf("card: what the bank said %s", a.Bank)
			}
		case current:
			seen++
			if string(a.Loan) != "null" || string(a.CreditCard) != "null" || string(a.Bank) != "null" {
				t.Errorf("current: loan %s, card %s, bank %s", a.Loan, a.CreditCard, a.Bank)
			}
		}
	}
	if seen != 3 {
		t.Errorf("saw %d of the 3 accounts", seen)
	}
}

// A safe box's terms, the bank's price and its bars go into the export as
// written in, the owner of a piece by username; other accounts carry none.
func TestTheExportCarriesValuables(t *testing.T) {
	s := newStack(t)
	box := post(t, s.c, s.url+"/api/v1/accounts", `{"name":"Ячейка","type":"valuables","currency":"RUB"}`, http.StatusCreated)["id"].(string)
	current := post(t, s.c, s.url+"/api/v1/accounts", `{"name":"Текущий","type":"checking","currency":"RUB"}`, http.StatusCreated)["id"].(string)
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
	put(s.url+"/api/v1/accounts/"+box+"/valuables/storage", `{"place":"safe_box","bank":"ВТБ","until":"2027-03-31","fee_minor":180000,"fee_period":"month","buyback_percent":"4"}`)
	put(s.url+"/api/v1/accounts/"+box+"/valuables/bank-prices", `{"metal":"gold","price":"10100","on":"2026-01-10"}`)
	me := get(t, s.c, s.url+"/api/v1/auth/me")
	var who struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.NewDecoder(me.Body).Decode(&who); err != nil {
		t.Fatal(err)
	}
	post(t, s.c, s.url+"/api/v1/accounts/"+box+"/valuables/items", `{"kind":"bar","metal":"gold","weight":"100","fineness":"999.9","maker":"Красцветмет",
		"foreign_made":false,"serial":"123456","certificate":true,"sealed":true,"bought_on":"2023-03-15","cost_minor":52000000,"currency":"RUB",
		"owner_user_id":"`+who.User.ID+`","note":"из ячейки"}`, http.StatusOK)

	var doc struct {
		Accounts []struct {
			ID        string          `json:"id"`
			Valuables json.RawMessage `json:"valuables"`
		} `json:"accounts"`
	}
	if err := json.NewDecoder(get(t, s.c, s.url+"/api/v1/export").Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	for _, a := range doc.Accounts {
		switch a.ID {
		case box:
			v := string(a.Valuables)
			for _, want := range []string{`"place":"safe_box"`, `"buyback_percent":"4.00"`, `"price":"10100"`, `"serial":"123456"`, `"owner_username":"alex"`, `"weight":"100"`, `"note":"из ячейки"`} {
				if !strings.Contains(v, want) {
					t.Errorf("valuables %s lacks %s", v, want)
				}
			}
			if strings.Contains(v, "market_minor") {
				t.Errorf("valuables carry a value: %s", v)
			}
		case current:
			if string(a.Valuables) != "null" {
				t.Errorf("current: %s", a.Valuables)
			}
		}
	}
}

// What the family planned is the family's too: its own country and sector of a
// paper no journal holds, a savings goal on a whole account and a sum out of
// another, a saved plan to buy that paper — and the paper with them — and a
// payment it said is not regular.
func TestTheExportCarriesThePlans(t *testing.T) {
	s := newStack(t)
	broker := post(t, s.c, s.url+"/api/v1/accounts", `{"name":"Брокер","type":"brokerage","currency":"RUB"}`, http.StatusCreated)["id"].(string)
	savings := post(t, s.c, s.url+"/api/v1/accounts", `{"name":"Накопительный","type":"savings","currency":"RUB"}`, http.StatusCreated)["id"].(string)
	paper := post(t, s.c, s.url+"/api/v1/instruments", `{"type":"etf","name":"FinEx FXUS","ticker":"FXUS","isin":"IE00BD3QHZ91","currency":"RUB"}`, http.StatusCreated)["id"].(string)
	req, _ := http.NewRequest(http.MethodPut, s.url+"/api/v1/instruments/"+paper+"/class", strings.NewReader(`{"country":"US","sector":null}`))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := s.c.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("class: %v %v", resp, err)
	}
	req, _ = http.NewRequest(http.MethodPut, s.url+"/api/v1/accounts/"+savings+"/rates", strings.NewReader(`{"from_on":"2026-09-01","kind":"fixed","percent":"16.5"}`))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := s.c.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("rate: %v %v", resp, err)
	}
	post(t, s.c, s.url+"/api/v1/allocation/plans", `{"name":"Октябрь","operations":[{"kind":"buy","account_id":"`+broker+`","instrument_id":"`+paper+`","currency":"RUB","amount_minor":10000000}]}`, http.StatusOK)

	var who struct {
		SpaceID string `json:"space_id"`
	}
	if err := json.NewDecoder(get(t, s.c, s.url+"/api/v1/auth/me").Body).Decode(&who); err != nil {
		t.Fatal(err)
	}
	target, part := int64(570_000_00), int64(70_000_00)
	if _, err := s.goals.Save(t.Context(), uuid.MustParse(who.SpaceID), goals.Goal{
		Name: "Подушка", Target: &target, Yield: decimal.NewFromInt(14),
		Funds: []goals.Fund{{Account: uuid.MustParse(savings)}, {Account: uuid.MustParse(broker), Minor: &part}},
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.hidden.Hide(t.Context(), uuid.MustParse(who.SpaceID), recurring.KeyOf("ИП Смирнова", false, "RUB")); err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Instruments []struct {
			ID string `json:"id"`
		} `json:"instruments"`
		Accounts []struct {
			Name  string           `json:"name"`
			Rates []map[string]any `json:"rates"`
		} `json:"accounts"`
		HiddenRegular     []map[string]any `json:"hidden_regular"`
		InstrumentClasses []map[string]any `json:"instrument_classes"`
		SavingsGoals      []map[string]any `json:"savings_goals"`
		AllocationPlans   []map[string]any `json:"allocation_plans"`
	}
	if err := json.NewDecoder(get(t, s.c, s.url+"/api/v1/export").Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.InstrumentClasses) != 1 || doc.InstrumentClasses[0]["country"] != "US" || doc.InstrumentClasses[0]["sector"] != nil {
		t.Errorf("classes = %v", doc.InstrumentClasses)
	}
	if len(doc.SavingsGoals) != 1 || doc.SavingsGoals[0]["name"] != "Подушка" || doc.SavingsGoals[0]["yield_percent"] != "14.00" ||
		len(doc.SavingsGoals[0]["funds"].([]any)) != 2 {
		t.Errorf("goals = %v", doc.SavingsGoals)
	}
	for _, a := range doc.Accounts {
		want := 0
		if a.Name == "Накопительный" {
			want = 1
		}
		if len(a.Rates) != want || want == 1 && (a.Rates[0]["percent"] != "16.500" || a.Rates[0]["from_on"] != "2026-09-01" || a.Rates[0]["by_hand"] != true) {
			t.Errorf("%s: rates = %v", a.Name, a.Rates)
		}
	}
	if len(doc.AllocationPlans) != 1 || doc.AllocationPlans[0]["name"] != "Октябрь" {
		t.Errorf("plans = %v", doc.AllocationPlans)
	}
	if len(doc.HiddenRegular) != 1 || doc.HiddenRegular[0]["payee"] != "ип смирнова" || doc.HiddenRegular[0]["incoming"] != false {
		t.Errorf("hidden = %v", doc.HiddenRegular)
	}
	if len(doc.Instruments) != 1 || doc.Instruments[0].ID != paper {
		t.Errorf("instruments = %v; the planned paper belongs with its plan", doc.Instruments)
	}
}
