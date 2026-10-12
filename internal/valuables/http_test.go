package valuables_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitest"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpserver"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/valuables"
)

// A safe box with the note's bar (Р-39): valued by the central bank's last
// price and as it would sell now; the bank's own price, once written in,
// stands for the guide; a piece corrected and taken off; what is refused.
func TestASafeBoxOverHTTP(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	accStore, md := account.NewStore(pool), marketdata.NewStore(pool)
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	account.NewHandler(accStore, famStore, marketdata.NewConverter(md), nil, auth, sm).Mount(srv)
	valuables.NewHandler(valuables.NewService(pool, accStore, md), auth, sm).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())

	mk := func(name, typ string) string {
		var a struct {
			ID string `json:"id"`
		}
		apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/accounts", fmt.Sprintf(`{"name":%q,"type":%q,"currency":"RUB"}`, name, typ)), &a)
		return a.ID
	}
	box, card := mk("Ячейка", "valuables"), mk("Карта", "checking")
	base := url + "/api/v1/accounts/" + box + "/valuables"

	var got apitypes.Valuables
	apitest.Decode(t, apitest.Do(t, c, "GET", base, ""), &got)
	if got.Storage.Place != "home" || len(got.Items) != 0 || got.MarketMinor.MustGet() != 0 || got.SellMinor.MustGet() != 0 {
		t.Fatalf("an empty storage = %+v", got)
	}
	if r := apitest.Do(t, c, "GET", url+"/api/v1/accounts/"+card+"/valuables", ""); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a card's valuables = %d, want 400", r.StatusCode)
	}

	storage := `{"place":"safe_box","bank":"ВТБ","until":"2027-03-31","fee_minor":180000,"fee_period":"month","buyback_percent":null}`
	apitest.Decode(t, apitest.Do(t, c, "PUT", base+"/storage", storage), &got)
	if got.Storage.Place != "safe_box" || got.Storage.Bank != "ВТБ" || got.Storage.Until.MustGet() != "2027-03-31" || got.Storage.FeeMinor.MustGet() != 180000 {
		t.Errorf("storage = %+v", got.Storage)
	}
	if r := apitest.Do(t, c, "PUT", base+"/storage", `{"place":"safe_box","bank":"","until":null,"fee_minor":100,"fee_period":null,"buyback_percent":null}`); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a fee with no period = %d, want 400", r.StatusCode)
	}

	bar := `{"kind":"bar","metal":"gold","weight":"100","fineness":"999.9","maker":"Красцветмет","foreign_made":false,"serial":"123456",
		"certificate":true,"sealed":true,"bought_on":"2023-03-15","cost_minor":52000000,"currency":"RUB","owner_user_id":null,"note":""}`
	apitest.Decode(t, apitest.Do(t, c, "POST", base+"/items", bar), &got)
	if len(got.Items) != 1 {
		t.Fatalf("items = %+v", got.Items)
	}
	it := got.Items[0]
	if it.PureGrams != "99.99" || it.SellBy != "no_price" || !it.MarketMinor.IsNull() || !got.SellMinor.IsNull() || it.TaxFreeFrom != "2026-03-15" || len(it.HardToSell) != 0 {
		t.Errorf("before the central bank's price: %+v, sell %v", it, got.SellMinor)
	}

	if err := md.UpsertMetalPrices(t.Context(), []marketdata.MetalPrice{
		{Metal: marketdata.Gold, On: time.Now().UTC().AddDate(0, 0, -1), Price: decimal.NewFromInt(10500), Source: "cbr"},
	}); err != nil {
		t.Fatal(err)
	}
	apitest.Decode(t, apitest.Do(t, c, "GET", base, ""), &got)
	it = got.Items[0]
	if it.MarketMinor.MustGet() != 1_049_895_00 || it.SellMinor.MustGet() != 99_740_025 || it.SellBy != "guide_percent" || it.BuybackPercent.MustGet() != "5.00" ||
		got.MarketMinor.MustGet() != 1_049_895_00 || got.SellMinor.MustGet() != 99_740_025 {
		t.Errorf("by the guide: %+v, totals %v %v", it, got.MarketMinor, got.SellMinor)
	}

	today := time.Now().UTC().Format(time.DateOnly)
	apitest.Decode(t, apitest.Do(t, c, "PUT", base+"/bank-prices", fmt.Sprintf(`{"metal":"gold","price":"10100","on":%q}`, today)), &got)
	if it := got.Items[0]; it.SellBy != "bank_price" || it.SellMinor.MustGet() != 1_009_899_00 || it.BankPriceOn.MustGet() != today {
		t.Errorf("by the bank's price: %+v", it)
	}
	if r := apitest.Do(t, c, "PUT", base+"/bank-prices", `{"metal":"tin","price":"1","on":"2026-01-01"}`); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a price of tin = %d, want 400", r.StatusCode)
	}

	// Corrected: it was PAMP, its certificate lost.
	pamp := `{"kind":"bar","metal":"gold","weight":"10","fineness":"999.9","maker":"PAMP","foreign_made":true,"serial":"A1",
		"certificate":false,"sealed":true,"bought_on":"2025-05-20","cost_minor":9500000,"currency":"RUB","owner_user_id":null,"note":""}`
	apitest.Decode(t, apitest.Do(t, c, "PUT", base+"/items/"+it.Id.String(), pamp), &got)
	if it := got.Items[0]; it.Maker != "PAMP" || len(it.HardToSell) != 2 || it.HardToSell[0] != "foreign_made" || it.TaxFreeFrom != "2028-05-20" {
		t.Errorf("corrected: %+v", it)
	}
	for _, bad := range []string{
		`{"kind":"bar","metal":"gold","weight":"0","fineness":"999.9","maker":"","foreign_made":false,"serial":"","certificate":true,"sealed":true,"bought_on":"2025-01-01","cost_minor":1,"currency":"RUB","owner_user_id":null,"note":""}`,
		`{"kind":"bar","metal":"gold","weight":"1","fineness":"1001","maker":"","foreign_made":false,"serial":"","certificate":true,"sealed":true,"bought_on":"2025-01-01","cost_minor":1,"currency":"RUB","owner_user_id":null,"note":""}`,
		`{"kind":"ring","metal":"gold","weight":"1","fineness":"999","maker":"","foreign_made":false,"serial":"","certificate":true,"sealed":true,"bought_on":"2025-01-01","cost_minor":1,"currency":"RUB","owner_user_id":null,"note":""}`,
		`{"kind":"bar","metal":"gold","weight":"1","fineness":"999","maker":"","foreign_made":false,"serial":"","certificate":true,"sealed":true,"bought_on":"2099-01-01","cost_minor":1,"currency":"RUB","owner_user_id":null,"note":""}`,
		`{"kind":"bar","metal":"gold","weight":"1","fineness":"999","maker":"","foreign_made":false,"serial":"","certificate":true,"sealed":true,"bought_on":"2025-01-01","cost_minor":1,"currency":"RUB","owner_user_id":"00000000-0000-0000-0000-000000000001","note":""}`,
	} {
		if r := apitest.Do(t, c, "POST", base+"/items", bad); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, r.StatusCode)
		}
	}
	if r := apitest.Do(t, c, "DELETE", url+"/api/v1/accounts/"+card+"/valuables/items/"+it.Id.String(), ""); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a piece off a card = %d, want 400", r.StatusCode)
	}
	apitest.Decode(t, apitest.Do(t, c, "DELETE", base+"/items/"+it.Id.String(), ""), &got)
	if len(got.Items) != 0 {
		t.Errorf("taken off: %+v", got.Items)
	}
	if r := apitest.Do(t, c, "DELETE", base+"/items/"+it.Id.String(), ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("taken off twice = %d, want 404", r.StatusCode)
	}
}

// A box's worth on days, as the family's total and capital chart read it: a
// bar counts from the day it was bought, at the central bank's price of that
// day or the nearest before; the liquid worth is as it would sell, the full
// one by the bank's price; in dollars at that day's rate when the family
// counts in dollars, left out and named when there is no rate.
func TestABoxIsWorthItsPiecesOnADay(t *testing.T) {
	pool := testdb.New(t)
	famStore := family.NewStore(pool)
	sm := family.NewSessionManager(pool)
	auth := family.NewAuth(sm, famStore)
	accStore, md := account.NewStore(pool), marketdata.NewStore(pool)
	svc := valuables.NewService(pool, accStore, md).WithWorth(famStore, md, marketdata.NewConverter(md))
	srv := httpserver.New(slog.Default(), pool)
	family.NewHandler(family.NewService(famStore), famStore, auth, sm).Mount(srv)
	account.NewHandler(accStore, famStore, marketdata.NewConverter(md), nil, auth, sm).Mount(srv)
	valuables.NewHandler(svc, auth, sm).Mount(srv)
	url, c := apitest.Serve(t, srv.Handler())

	var a struct {
		ID string `json:"id"`
	}
	apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/accounts", `{"name":"Дом","type":"valuables","currency":"RUB"}`), &a)
	var card struct {
		ID string `json:"id"`
	}
	apitest.Decode(t, apitest.Do(t, c, "POST", url+"/api/v1/accounts", `{"name":"Карта","type":"checking","currency":"RUB"}`), &card)
	for _, body := range []string{
		`{"kind":"bar","metal":"gold","weight":"100","fineness":"1000","maker":"","foreign_made":false,"serial":"","certificate":true,"sealed":true,"bought_on":"2026-01-10","cost_minor":1,"currency":"RUB","owner_user_id":null,"note":""}`,
		`{"kind":"bar","metal":"silver","weight":"1000","fineness":"1000","maker":"","foreign_made":false,"serial":"","certificate":true,"sealed":true,"bought_on":"2026-03-01","cost_minor":1,"currency":"RUB","owner_user_id":null,"note":""}`,
	} {
		if r := apitest.Do(t, c, "POST", url+"/api/v1/accounts/"+a.ID+"/valuables/items", body); r.StatusCode != http.StatusOK {
			t.Fatalf("add = %d", r.StatusCode)
		}
	}
	on := func(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d }
	if err := md.UpsertMetalPrices(t.Context(), []marketdata.MetalPrice{
		{Metal: marketdata.Gold, On: on("2026-01-09"), Price: decimal.NewFromInt(10000), Source: "cbr"},
		{Metal: marketdata.Gold, On: on("2026-03-02"), Price: decimal.NewFromInt(11000), Source: "cbr"},
		{Metal: marketdata.Silver, On: on("2026-03-02"), Price: decimal.NewFromInt(150), Source: "cbr"},
	}); err != nil {
		t.Fatal(err)
	}
	box := parseID(t, a.ID)
	got, ok, err := svc.Worths(t.Context(), spaceOf(t, pool), box, []time.Time{on("2026-01-05"), on("2026-01-15"), on("2026-03-01"), on("2026-03-05")})
	if err != nil || !ok {
		t.Fatalf("worths: %v, %v", ok, err)
	}
	// Before the bar; the bar at 10 000 (5 % off: 950 000); the silver bought
	// the day before its first price — not valued; both at the new prices.
	want := []struct {
		ops, unpriced    int
		minor, fullMinor int64
	}{
		{0, 0, 0, 0},
		{1, 0, 95_000_000, 100_000_000},
		{2, 1, 95_000_000, 100_000_000},
		{2, 0, 104_500_000 + 11_250_000, 110_000_000 + 15_000_000},
	}
	for i, w := range want {
		g := got[i]
		if g.Operations != w.ops || g.Unpriced != w.unpriced || g.Minor != w.minor || g.FullMinor != w.fullMinor || g.Currency != "RUB" {
			t.Errorf("day %d = %+v, want %+v", i, g, w)
		}
	}
	if _, ok, err := svc.Worths(t.Context(), spaceOf(t, pool), parseID(t, card.ID), []time.Time{on("2026-03-05")}); ok || err != nil {
		t.Errorf("a card is no box: %v, %v", ok, err)
	}

	// Counted in dollars: at 100 roubles a dollar, then with no rate.
	if r := apitest.Do(t, c, "PATCH", url+"/api/v1/space", `{"base_currency":"USD"}`); r.StatusCode != http.StatusOK {
		t.Fatalf("base currency = %d", r.StatusCode)
	}
	got, _, err = svc.Worths(t.Context(), spaceOf(t, pool), box, []time.Time{on("2026-03-05")})
	if err != nil || len(got[0].MissingRates) != 1 || got[0].Minor != 0 || got[0].Currency != "USD" {
		t.Errorf("no rate: %+v, %v", got, err)
	}
	if err := md.UpsertFxRates(t.Context(), []marketdata.FxRate{{Base: "USD", Quote: "RUB", On: on("2026-03-01"), Rate: decimal.NewFromInt(100), Source: "test"}}); err != nil {
		t.Fatal(err)
	}
	got, _, err = svc.Worths(t.Context(), spaceOf(t, pool), box, []time.Time{on("2026-03-05")})
	if err != nil || got[0].Minor != 1_157_500 || got[0].FullMinor != 1_250_000 || got[0].ByCurrency["RUB"] != 115_750_000 {
		t.Errorf("in dollars: %+v, %v", got, err)
	}
}

func parseID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// spaceOf is the one space the test signed up.
func spaceOf(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT id FROM spaces LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
