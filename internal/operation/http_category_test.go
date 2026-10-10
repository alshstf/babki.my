package operation_test

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/category"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/apitest"
)

type categorized struct {
	ID            string  `json:"id"`
	Categorizable bool    `json:"categorizable"`
	CategoryID    *string `json:"category_id"`
	Counterparty  string  `json:"counterparty"`
}

// categoryIDs reads the family's categories by name, the default set filed on
// the first look.
func categoryIDs(t *testing.T, url string, c *http.Client) map[string]string {
	t.Helper()
	resp := apitest.Do(t, c, "GET", url+"/api/v1/categories", "")
	var list []struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	apitest.Decode(t, resp, &list)
	out := map[string]string{}
	for _, c := range list {
		out[c.Kind+"/"+c.Name] = c.ID
	}
	return out
}

func status(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	return fmt.Sprintf("%d %s", resp.StatusCode, b)
}

// A spending is a withdrawal with a spending category, an earning a deposit
// with an earning one; the row keeps who the money went to.
func TestARowTakesACategoryOfItsDirection(t *testing.T) {
	url, c := newAPI(t)
	acc := mkAccount(t, url, c, "Карта", "RUB")
	cats := categoryIDs(t, url, c)
	mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"deposit","occurred_on":"2026-09-01","amount_minor":10000000,"currency":"RUB","category_id":%q,"counterparty":"  ООО Работа "}`,
		acc, cats["income/Зарплата"]))

	resp := apitest.Do(t, c, "POST", url+"/api/v1/operations", fmt.Sprintf(
		`{"account_id":%q,"type":"withdrawal","occurred_on":"2026-09-02","amount_minor":-250000,"currency":"RUB","category_id":%q,"counterparty":"Пятёрочка"}`,
		acc, cats["expense/Продукты"]))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("a spending on groceries = %s", status(t, resp))
	}
	var spent categorized
	apitest.Decode(t, resp, &spent)
	if !spent.Categorizable || spent.CategoryID == nil || *spent.CategoryID != cats["expense/Продукты"] || spent.Counterparty != "Пятёрочка" {
		t.Errorf("the spending as stored: %+v", spent)
	}

	for name, body := range map[string]string{
		"a withdrawal under an earning category": fmt.Sprintf(`"type":"withdrawal","amount_minor":-100,"category_id":%q`, cats["income/Зарплата"]),
		"a deposit under a spending category":    fmt.Sprintf(`"type":"deposit","amount_minor":100,"category_id":%q`, cats["expense/Продукты"]),
		"a category of nobody's":                 fmt.Sprintf(`"type":"withdrawal","amount_minor":-100,"category_id":%q`, uuid.NewString()),
		"a counterparty past 200 characters":     fmt.Sprintf(`"type":"withdrawal","amount_minor":-100,"counterparty":%q`, string(make([]rune, 201))),
	} {
		resp := apitest.Do(t, c, "POST", url+"/api/v1/operations",
			fmt.Sprintf(`{"account_id":%q,"occurred_on":"2026-09-03","currency":"RUB",%s}`, acc, body))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %s, want 400", name, status(t, resp))
		}
	}
}

// A category can be put on a row and taken off it on its own, a broker's row
// too; not on a trade, and an archived one only where the row has it already.
func TestACategoryIsSetOnARowOfItsOwn(t *testing.T) {
	url, c := newAPI(t)
	acc := mkAccount(t, url, c, "Брокер", "RUB")
	cats := categoryIDs(t, url, c)
	sber := mkInstrument(t, url, c, `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	fee := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"deposit","occurred_on":"2026-09-01","amount_minor":1000000,"currency":"RUB"}`, acc))
	buy := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2026-09-02","quantity":"1","price":"300","currency":"RUB"}`, acc, sber))
	put := func(id, body string) *http.Response {
		return apitest.Do(t, c, "PUT", url+"/api/v1/operations/"+id+"/category", body)
	}

	resp := put(fee, fmt.Sprintf(`{"category_id":%q}`, cats["income/Подарки"]))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("filing a deposit = %s", status(t, resp))
	}
	var row categorized
	apitest.Decode(t, resp, &row)
	if row.CategoryID == nil || *row.CategoryID != cats["income/Подарки"] {
		t.Errorf("the deposit after filing: %+v", row)
	}

	for name, r := range map[string]*http.Response{
		"a buy":                put(buy, fmt.Sprintf(`{"category_id":%q}`, cats["expense/Продукты"])),
		"no category_id":       put(fee, `{}`),
		"the wrong direction":  put(fee, fmt.Sprintf(`{"category_id":%q}`, cats["expense/Продукты"])),
		"another family's row": put(uuid.NewString(), `{"category_id":null}`),
	} {
		if r.StatusCode != http.StatusBadRequest && r.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %s, want 400 or 404", name, status(t, r))
		}
	}

	// Archived after the filing: the row keeps it, a new filing may not use it.
	if r := apitest.Do(t, c, "PATCH", url+"/api/v1/categories/"+cats["income/Подарки"], `{"archived":true}`); r.StatusCode != http.StatusOK {
		t.Fatalf("archive = %s", status(t, r))
	}
	if r := put(fee, fmt.Sprintf(`{"category_id":%q}`, cats["income/Подарки"])); r.StatusCode != http.StatusOK {
		t.Errorf("keeping an archived category = %s", status(t, r))
	}
	other := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"type":"deposit","occurred_on":"2026-09-03","amount_minor":5000,"currency":"RUB"}`, acc))
	if r := put(other, fmt.Sprintf(`{"category_id":%q}`, cats["income/Подарки"])); r.StatusCode != http.StatusBadRequest {
		t.Errorf("filing under an archived category = %s, want 400", status(t, r))
	}

	// A category in use is not removed; taken off, it is.
	if r := apitest.Do(t, c, "DELETE", url+"/api/v1/categories/"+cats["income/Подарки"], ""); r.StatusCode == http.StatusNoContent {
		t.Error("a category in use was removed")
	}
	if r := put(fee, `{"category_id":null}`); r.StatusCode != http.StatusOK {
		t.Fatalf("taking it off = %s", status(t, r))
	}
	if r := apitest.Do(t, c, "DELETE", url+"/api/v1/categories/"+cats["income/Подарки"], ""); r.StatusCode != http.StatusNoContent {
		t.Errorf("removing an unused category = %s", status(t, r))
	}
}

// The journal narrows to a category with the ones inside it, or to the rows
// still waiting for one.
func TestTheJournalIsFilteredByCategory(t *testing.T) {
	url, c := newAPI(t)
	acc := mkAccount(t, url, c, "Карта", "RUB")
	cats := categoryIDs(t, url, c)
	op := func(body string) {
		mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"occurred_on":"2026-09-01","currency":"RUB",%s}`, acc, body))
	}
	op(`"type":"deposit","amount_minor":10000000`)
	op(fmt.Sprintf(`"type":"withdrawal","amount_minor":-50000,"category_id":%q`, cats["expense/Такси"]))
	op(fmt.Sprintf(`"type":"withdrawal","amount_minor":-60000,"category_id":%q`, cats["expense/Транспорт"]))
	op(fmt.Sprintf(`"type":"withdrawal","amount_minor":-70000,"category_id":%q`, cats["expense/Продукты"]))
	op(`"type":"fee","amount_minor":-100`)

	count := func(query string) int {
		t.Helper()
		resp := apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+acc+"/operations?"+query, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %s", query, status(t, resp))
		}
		var page struct {
			Operations []struct{} `json:"operations"`
		}
		apitest.Decode(t, resp, &page)
		return len(page.Operations)
	}
	for query, want := range map[string]int{
		"category=" + cats["expense/Транспорт"]: 2,
		"category=" + cats["expense/Такси"]:     1,
		"category=none":                         2,
		"category=none&type=fee":                1,
	} {
		if got := count(query); got != want {
			t.Errorf("%q gave %d operations, want %d", query, got, want)
		}
	}
	for _, query := range []string{"category=" + uuid.NewString(), "category=taxi"} {
		if resp := apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+acc+"/operations?"+query, ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q = %d, want 400", query, resp.StatusCode)
		}
	}
}

// A broker's corrected record keeps the category the family gave the one it
// replaces; a record of another type does not.
func TestACorrectedRecordKeepsItsCategory(t *testing.T) {
	f := newFixture(t)
	cats, err := category.NewStore(f.pool).List(f.ctx, f.spaceID)
	if err != nil {
		t.Fatal(err)
	}
	var gifts uuid.UUID
	for _, c := range cats {
		if c.Kind == category.KindIncome && c.Name == "Подарки" {
			gifts = c.ID
		}
	}
	existing, err := f.store.Create(f.ctx, f.spaceID, importedDeposit(f, "op-1", "2026-07-01", 1_000), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operation.NewService(f.store).SetCategory(f.ctx, f.spaceID, existing.ID, &gifts); err != nil {
		t.Fatal(err)
	}
	stored, err := f.store.ApplyDelta(f.ctx, f.spaceID,
		[]operation.Operation{importedDeposit(f, "op-1", "2026-07-01", 1_500), importedDeposit(f, "op-2", "2026-07-02", 700)},
		[]uuid.UUID{existing.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stored[0].CategoryID == nil || *stored[0].CategoryID != gifts {
		t.Errorf("the corrected record lost its category: %v", stored[0].CategoryID)
	}
	if stored[1].CategoryID != nil {
		t.Errorf("a new record took a category: %v", stored[1].CategoryID)
	}
}

// Rules file the unfiled rows of everyday accounts and leave a broker's, a
// filed row and a row no rule fits alone.
func TestRulesFileTheUnfiledRows(t *testing.T) {
	url, c := newAPI(t)
	card := mkAccountOfType(t, url, c, "Карта", "checking")
	broker := mkAccount(t, url, c, "Брокер", "RUB")
	cats := categoryIDs(t, url, c)
	for _, rule := range []string{
		fmt.Sprintf(`{"category_id":%q,"field":"counterparty","pattern":"пятёрочка"}`, cats["expense/Продукты"]),
		fmt.Sprintf(`{"category_id":%q,"field":"any","pattern":"пополнение"}`, cats["income/Подарки"]),
	} {
		if r := apitest.Do(t, c, "POST", url+"/api/v1/category-rules", rule); r.StatusCode != http.StatusCreated {
			t.Fatalf("rule %s = %s", rule, status(t, r))
		}
	}
	op := func(acc, body string) string {
		return mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"occurred_on":"2026-09-01","currency":"RUB",%s}`, acc, body))
	}
	op(card, `"type":"deposit","amount_minor":10000000`)
	shop := op(card, `"type":"withdrawal","amount_minor":-50000,"counterparty":"ПЯТЕРОЧКА 4411"`)
	kept := op(card, fmt.Sprintf(`"type":"withdrawal","amount_minor":-60000,"counterparty":"Пятёрочка","category_id":%q`, cats["expense/Кафе и рестораны"]))
	op(card, `"type":"withdrawal","amount_minor":-70000,"counterparty":"Лента"`)
	brokerDeposit := op(broker, `"type":"deposit","amount_minor":100000,"note":"Пополнение счёта"`)

	resp := apitest.Do(t, c, "POST", url+"/api/v1/operations/file-by-rules", `{}`)
	var out struct {
		Filed int `json:"filed"`
	}
	apitest.Decode(t, resp, &out)
	if out.Filed != 1 {
		t.Errorf("filed %d rows, want the one at Пятёрочка", out.Filed)
	}
	rows := map[string]categorized{}
	for _, acc := range []string{card, broker} {
		var page struct {
			Operations []categorized `json:"operations"`
		}
		apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+acc+"/operations", ""), &page)
		for _, o := range page.Operations {
			rows[o.ID] = o
		}
	}
	if r := rows[shop]; r.CategoryID == nil || *r.CategoryID != cats["expense/Продукты"] {
		t.Errorf("the shop row: %+v", r)
	}
	if r := rows[kept]; *r.CategoryID != cats["expense/Кафе и рестораны"] {
		t.Errorf("a filed row was refiled: %+v", r)
	}
	if r := rows[brokerDeposit]; r.CategoryID != nil {
		t.Errorf("the broker's deposit was filed: %+v", r)
	}
}

// mkAccountOfType creates a RUB account of the given type.
func mkAccountOfType(t *testing.T, url string, c *http.Client, name, typ string) string {
	t.Helper()
	resp := apitest.Do(t, c, "POST", url+"/api/v1/accounts", fmt.Sprintf(`{"name":%q,"type":%q,"currency":"RUB"}`, name, typ))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %s account = %s", typ, status(t, resp))
	}
	var a idResp
	apitest.Decode(t, resp, &a)
	return a.ID
}
