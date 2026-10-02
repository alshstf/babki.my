package table_test

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

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/importer/table"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

func newAPI(t *testing.T) (string, *http.Client) {
	t.Helper()
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	conv := marketdata.NewConverter(marketdata.NewStore(pool))
	accStore, instStore, opStore := account.NewStore(pool), instrument.NewStore(pool), operation.NewStore(pool)
	opSvc := operation.NewService(opStore)

	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	account.NewHandler(accStore, famStore, conv, nil, auth, sm).Mount(srv)
	instrument.NewHandler(instStore, auth, sm).Mount(srv)
	operation.NewHandler(opSvc, opStore, famStore, conv, auth, sm).Mount(srv)
	table.NewHandler(table.NewService(accStore, instStore, opStore, opSvc), auth, sm).Mount(srv)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.Post(ts.URL+"/api/v1/setup", "application/json",
		strings.NewReader(`{"space_name":"S","username":"alex","display_name":"A","password":"secret123"}`))
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("setup: %v %d", err, resp.StatusCode)
	}
	return ts.URL, c
}

func call(t *testing.T, c *http.Client, method, url, body string, want int, out any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, url, resp.StatusCode, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	}
}

type preview struct {
	Mapping struct {
		HasHeader bool              `json:"has_header"`
		Columns   map[string]int    `json:"columns"`
		Types     map[string]string `json:"types"`
	} `json:"mapping"`
	Header []string `json:"header"`
	Rows   []struct {
		Line      int     `json:"line"`
		Verdict   string  `json:"verdict"`
		Reason    *string `json:"reason"`
		Operation *struct {
			Type         string  `json:"type"`
			OccurredOn   string  `json:"occurred_on"`
			InstrumentID *string `json:"instrument_id"`
			Quantity     *string `json:"quantity"`
			AmountMinor  int64   `json:"amount_minor"`
			Currency     string  `json:"currency"`
			FeeMinor     int64   `json:"fee_minor"`
		} `json:"operation"`
	} `json:"rows"`
}

// A broker's export read against an account: each row says what importing it
// would do, the journal is asked about the new ones as an import would ask it,
// and nothing is written.
func TestATableIsPreviewedRowByRow(t *testing.T) {
	url, c := newAPI(t)
	var acc, sber struct {
		ID string `json:"id"`
	}
	call(t, c, "POST", url+"/api/v1/accounts", `{"name":"Альфа","type":"brokerage","currency":"RUB"}`, 201, &acc)
	call(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"share","name":"Сбербанк","ticker":"SBER","isin":"RU0009029540","currency":"RUB"}`, 201, &sber)

	csv := "Дата;Операция;Бумага;Количество;Цена;Сумма;Комиссия\n" +
		"01.07.2026;Пополнение;;;;10 000,00;\n" + // line 2
		"10.07.2026;Покупка;SBER;10;300;;1,50\n" + // line 3: amount from quantity × price
		"11.07.2026;Покупка;RU0009029540;5;310;1 550,00;\n" + // line 4: by ISIN
		"12.07.2026;Продажа;SBER;100;320;32 000,00;\n" + // line 5: more than held
		"13.07.2026;Покупка;GAZP;1;150;150;\n" + // line 6: not in the catalog
		"32.07.2026;Покупка;SBER;1;300;300;\n" + // line 7: no such date
		"14.07.2026;Перевод;;;;100;\n" // line 8: type not mapped
	body, _ := json.Marshal(map[string]string{"content": csv})
	var got preview
	call(t, c, "POST", url+"/api/v1/accounts/"+acc.ID+"/imports/preview", string(body), 200, &got)

	if !got.Mapping.HasHeader || got.Mapping.Columns["instrument"] != 2 || got.Mapping.Types["покупка"] != "buy" {
		t.Errorf("mapping = %+v, want the header read and покупка recognized", got.Mapping)
	}
	want := map[int]string{2: "new", 3: "new", 4: "new", 5: "refused", 6: "unparsed", 7: "unparsed", 8: "unparsed"}
	for _, row := range got.Rows {
		if row.Verdict != want[row.Line] {
			reason := ""
			if row.Reason != nil {
				reason = *row.Reason
			}
			t.Errorf("line %d = %s (%s), want %s", row.Line, row.Verdict, reason, want[row.Line])
		}
	}
	if len(got.Rows) != len(want) {
		t.Fatalf("%d rows, want %d", len(got.Rows), len(want))
	}
	buy := got.Rows[1].Operation
	if buy == nil || buy.AmountMinor != -300_000 || buy.FeeMinor != 150 || buy.InstrumentID == nil || *buy.InstrumentID != sber.ID {
		t.Errorf("line 3 reads as %+v, want a buy of SBER for −3 000 ₽ with a 1,50 ₽ fee", buy)
	}
	if dep := got.Rows[0].Operation; dep == nil || dep.AmountMinor != 1_000_000 || dep.Currency != "RUB" {
		t.Errorf("line 2 reads as %+v, want a deposit of 10 000 ₽", dep)
	}
	if r := got.Rows[3].Reason; r == nil || !strings.Contains(*r, "quantity") {
		t.Errorf("line 5's reason = %v, want the journal's own words about the quantity", r)
	}

	var journal struct {
		Operations []struct{} `json:"operations"`
	}
	call(t, c, "GET", url+"/api/v1/accounts/"+acc.ID+"/operations", "", 200, &journal)
	if len(journal.Operations) != 0 {
		t.Errorf("the preview wrote %d operations, want none", len(journal.Operations))
	}
}

// A mapping sent back is the one used; a type a table may not hold is refused.
func TestTheMappingSentIsTheOneUsed(t *testing.T) {
	url, c := newAPI(t)
	var acc struct {
		ID string `json:"id"`
	}
	call(t, c, "POST", url+"/api/v1/accounts", `{"name":"Счёт","type":"brokerage","currency":"RUB"}`, 201, &acc)
	csv := "2026-07-01|in|500\n"
	csv = strings.ReplaceAll(csv, "|", ";")
	mapping := `{"has_header":false,"columns":{"date":0,"type":1,"amount":2},"types":{"IN":"deposit"}}`
	var got preview
	call(t, c, "POST", url+"/api/v1/accounts/"+acc.ID+"/imports/preview",
		fmt.Sprintf(`{"content":%q,"mapping":%s}`, csv, mapping), 200, &got)
	if len(got.Rows) != 1 || got.Rows[0].Verdict != "new" || got.Rows[0].Operation.AmountMinor != 50_000 {
		t.Errorf("rows = %+v, want one deposit of 500", got.Rows)
	}
	call(t, c, "POST", url+"/api/v1/accounts/"+acc.ID+"/imports/preview",
		fmt.Sprintf(`{"content":%q,"mapping":%s}`, csv,
			`{"has_header":false,"columns":{"date":0,"type":1,"amount":2},"types":{"in":"transfer_in"}}`), 400, nil)
}
