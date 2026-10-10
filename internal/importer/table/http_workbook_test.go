package table_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The template downloads as a CSV and as a workbook; the workbook, sent back
// in base64, is read and imported like any table — and a request bigger than
// a JSON body may be still carries a file of a few megabytes.
func TestAWorkbookIsImported(t *testing.T) {
	url, c := newAPI(t)
	var acc struct {
		ID string `json:"id"`
	}
	call(t, c, "POST", url+"/api/v1/accounts", `{"name":"Альфа","type":"brokerage","currency":"RUB"}`, 201, &acc)
	call(t, c, "POST", url+"/api/v1/instruments",
		`{"type":"share","name":"Сбербанк","ticker":"SBER","isin":"RU0009029540","currency":"RUB"}`, 201, nil)

	get := func(format string, want int) (string, []byte) {
		t.Helper()
		resp, err := c.Get(url + "/api/v1/imports/template?format=" + format)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("template %s = %d, want %d", format, resp.StatusCode, want)
		}
		return resp.Header.Get("Content-Type"), body
	}
	if typ, body := get("csv", http.StatusOK); !strings.HasPrefix(typ, "text/csv") || !strings.Contains(string(body), "Дата;Тип;Бумага") {
		t.Errorf("csv template = %s %.60q", typ, body)
	}
	get("pdf", http.StatusBadRequest)
	typ, workbook := get("xlsx", http.StatusOK)
	if !strings.Contains(typ, "spreadsheetml") {
		t.Errorf("xlsx template type = %s", typ)
	}

	content := base64.StdEncoding.EncodeToString(workbook)
	var got preview
	body, _ := json.Marshal(map[string]any{"content": content, "format": "xlsx"})
	call(t, c, "POST", url+"/api/v1/accounts/"+acc.ID+"/imports/preview", string(body), 200, &got)
	for _, row := range got.Rows {
		if row.Verdict != "new" {
			t.Errorf("template line %d = %s %+v", row.Line, row.Verdict, row.Reason)
		}
	}
	var res struct {
		Import struct {
			RowsWritten int `json:"rows_written"`
		} `json:"import"`
	}
	body, _ = json.Marshal(map[string]any{"content": content, "format": "xlsx", "mapping": got.Mapping, "file_name": "шаблон.xlsx"})
	call(t, c, "POST", url+"/api/v1/accounts/"+acc.ID+"/imports", string(body), 200, &res)
	if res.Import.RowsWritten != len(got.Rows) || len(got.Rows) != 8 {
		t.Errorf("written %d of %d rows", res.Import.RowsWritten, len(got.Rows))
	}

	// Three megabytes of base64 pass where a JSON body stops at one; what is
	// not a workbook is refused as a table that cannot be read.
	big, _ := json.Marshal(map[string]any{"content": strings.Repeat("A", 3<<20), "format": "xlsx"})
	call(t, c, "POST", url+"/api/v1/accounts/"+acc.ID+"/imports/preview", string(big), 400, nil)
	odd, _ := json.Marshal(map[string]any{"content": "x", "format": "pdf"})
	call(t, c, "POST", url+"/api/v1/accounts/"+acc.ID+"/imports/preview", string(odd), 400, nil)
}
