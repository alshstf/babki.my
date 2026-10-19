package cashback_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/cashback"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
)

// Decision Р-58: a rouble card is tied to ВТБ's «Мультибонус» — a broker's
// account cannot be; the family picks October's categories at the bank's
// percents — more than the variant's and one, an unknown category, a percent
// of 0 are refused; October's purchases are told into them by their codes —
// September's, a deposit and one without a code are not, the last named;
// Т-Банк's cap holds a card's bonus; untied, the card has none.
func TestACardsCashback(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	accStore, opStore := account.NewStore(pool), operation.NewStore(pool)
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	account.NewHandler(accStore, famStore, nil, nil, auth, sm).Mount(srv)
	svc := cashback.NewService(cashback.NewStore(pool), accStore, opStore)
	cashback.NewHandler(svc, auth, sm).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())

	mk := func(name, typ string) string {
		var a struct {
			ID string `json:"id"`
		}
		apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/accounts", fmt.Sprintf(`{"name":%q,"type":%q,"currency":"RUB"}`, name, typ)), &a)
		return a.ID
	}
	card, black, broker := mk("Карта ВТБ", "checking"), mk("Black", "checking"), mk("Брокер", "brokerage")
	var space uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT id FROM spaces LIMIT 1`).Scan(&space); err != nil {
		t.Fatal(err)
	}
	spend := func(acc, on string, minor int64, mcc string) {
		t.Helper()
		d, _ := time.Parse(time.DateOnly, on)
		typ := operation.TypeWithdrawal
		if minor > 0 {
			typ = operation.TypeDeposit
		}
		if _, err := opStore.Create(t.Context(), space, operation.Operation{AccountID: uuid.MustParse(acc), Type: typ, OccurredOn: d, AmountMinor: minor, Currency: "RUB", MCC: mcc}, nil); err != nil {
			t.Fatal(err)
		}
	}

	var got apitypes.CardCashback
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+card+"/cashback", ""), &got)
	if !got.Programme.IsNull() {
		t.Fatalf("before = %+v", got)
	}
	if r := apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+broker+"/cashback/programme", `{"programme_id":"vtb-multibonus","variant_id":"multicard"}`); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a broker's account = %d, want 400", r.StatusCode)
	}
	if r := apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+card+"/cashback/programme", `{"programme_id":"vtb-multibonus","variant_id":"nope"}`); r.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown variant = %d, want 400", r.StatusCode)
	}
	apitest.Decode(t, apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+card+"/cashback/programme", `{"programme_id":"vtb-multibonus","variant_id":"multicard"}`), &got)
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+card+"/cashback?on=2026-10-20", ""), &got)
	if p := got.Programme.MustGet(); p.BankName != "ВТБ" || p.Pick != 3 || p.Of != 8 || p.Period != "month" || len(p.Categories) < 30 {
		t.Fatalf("tied = %+v", p)
	}

	picks := url + "/api/v1/accounts/" + card + "/cashback/picks"
	for name, body := range map[string]string{
		"five picks for three":  `{"on":"2026-10-15","picks":[{"category_id":"supermarkets","percent":"5"},{"category_id":"pharmacy","percent":"3"},{"category_id":"fuel","percent":"7"},{"category_id":"taxi","percent":"5"},{"category_id":"pets","percent":"5"}]}`,
		"an unknown category":   `{"on":"2026-10-15","picks":[{"category_id":"moon","percent":"5"}]}`,
		"a percent of 0":        `{"on":"2026-10-15","picks":[{"category_id":"pharmacy","percent":"0"}]}`,
		"a category twice":      `{"on":"2026-10-15","picks":[{"category_id":"pharmacy","percent":"3"},{"category_id":"pharmacy","percent":"4"}]}`,
		"thousandths of a cent": `{"on":"2026-10-15","picks":[{"category_id":"pharmacy","percent":"3.333"}]}`,
	} {
		if r := apitest.Do(t, c, "PUT", picks, body); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, r.StatusCode)
		}
	}
	spend(card, "2026-10-02", -1_000_000, "5411")
	spend(card, "2026-10-05", -50_000, "5912")
	spend(card, "2026-09-30", -200_000, "5541")
	spend(card, "2026-10-06", -30_000, "")
	spend(card, "2026-10-07", 100_000, "")
	apitest.Decode(t, apitest.Do(t, c, "PUT", picks, `{"on":"2026-10-15","picks":[{"category_id":"supermarkets","percent":"5"},{"category_id":"pharmacy","percent":"3"},{"category_id":"fuel","percent":"7"}]}`), &got)
	if got.PeriodFrom.MustGet() != "2026-10-01" || got.PeriodTo.MustGet() != "2026-10-31" || len(got.Lines) != 3 {
		t.Fatalf("picked = %+v", got)
	}
	want := map[string][2]int64{"fuel": {0, 0}, "supermarkets": {1_000_000, 50_000}, "pharmacy": {50_000, 1_500}}
	for _, l := range got.Lines {
		if w := want[l.CategoryId]; l.SpentMinor != w[0] || l.BonusMinor != w[1] {
			t.Errorf("%s = spent %d, bonus %d, want %v", l.CategoryId, l.SpentMinor, l.BonusMinor, w)
		}
	}
	if got.Lines[0].CategoryId != "fuel" || got.BonusMinor != 51_500 || got.UncodedMinor != 30_000 || got.UncodedCount != 1 || got.Capped {
		t.Errorf("October = %+v", got)
	}
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+card+"/cashback?on=2026-09-10", ""), &got)
	if len(got.Lines) != 0 || got.BonusMinor != 0 {
		t.Errorf("September, nothing picked = %+v", got)
	}

	apitest.Decode(t, apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+black+"/cashback/programme", `{"programme_id":"tbank-black","variant_id":"black"}`), &got)
	spend(black, "2026-10-03", -1_500_000, "5411")
	spend(black, "2026-10-04", -100_000, "5999")
	apitest.Decode(t, apitest.Do(t, c, "PUT", url+"/api/v1/accounts/"+black+"/cashback/picks", `{"on":"2026-10-03","picks":[{"category_id":"supermarkets","percent":"30"},{"category_id":"all","percent":"1"}]}`), &got)
	// The supermarket's purchase goes where it earns the most; one of no
	// category of its own — to «1 % на все покупки».
	if got.BonusMinor != 300_000 || !got.Capped || got.Lines[0].BonusMinor != 450_000 || got.Lines[1].SpentMinor != 100_000 || got.Lines[1].BonusMinor != 1_000 {
		t.Errorf("Black past its cap = %+v", got)
	}

	// October is picked on both cards; November waits on both (Р-58: the
	// family is asked at the period's start).
	if due, err := svc.Due(t.Context(), space, time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)); err != nil || len(due) != 0 {
		t.Errorf("October's due = %+v, %v", due, err)
	}
	if due, err := svc.Due(t.Context(), space, time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)); err != nil || len(due) != 2 || due[0].Name != "Black" || due[0].From.Format(time.DateOnly) != "2026-11-01" {
		t.Errorf("November's due = %+v, %v", due, err)
	}

	apitest.Decode(t, apitest.Do(t, c, "DELETE", url+"/api/v1/accounts/"+card+"/cashback/programme", ""), &got)
	if !got.Programme.IsNull() {
		t.Errorf("untied = %+v", got)
	}
	if r := apitest.Do(t, c, "DELETE", url+"/api/v1/accounts/"+card+"/cashback/programme", ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("untied twice = %d, want 404", r.StatusCode)
	}
	var list []apitypes.LoyaltyProgramme
	apitest.Decode(t, apitest.Do(t, c, "GET", url+"/api/v1/loyalty-programmes", ""), &list)
	if len(list) < 2 || list[0].BankName == "" {
		t.Errorf("catalog = %+v", list)
	}
}
