package portfolio_test

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/portfolio"
)

// What an account is worth by its journal: every open holding at its market
// value, the cash its operations leave, each currency converted once at
// today's rate. What cannot be counted is named beside the figure — a holding
// with no quote, a currency with no rate, cash the journal sends below zero.
func TestAnAccountIsValuedFromItsJournal(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	conv := marketdata.NewConverter(mdStore)
	url, c := setupAPI(t, pool, quotes, conv)
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: mustDate(t, earlyRateOn), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	short := createAccount(t, c, url, `{"name":"Без пополнения","type":"brokerage","currency":"RUB"}`)
	acme := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	beta := createInstrument(t, c, url, `{"type":"share","name":"Бета","ticker":"BETA","currency":"USD"}`)
	sber := createInstrument(t, c, url, `{"type":"share","name":"Сбербанк","ticker":"SBER","currency":"RUB"}`)
	gamma := createInstrument(t, c, url, `{"type":"share","name":"Гамма","ticker":"GAMMA","currency":"RUB"}`)
	// GAMMA, closed out below, has no quote: a closed position is not a holding
	// with no price and must not be counted as one.
	for id, q := range map[string][2]string{acme.ID: {"120", "USD"}, sber.ID: {"310", "RUB"}} {
		quotes.byInstrument[uuid.MustParse(id)] = marketdata.Quote{
			InstrumentID: uuid.MustParse(id), On: mustDate(t, "2026-07-28"),
			Price: decimal.RequireFromString(q[0]), Currency: q[1],
		}
	}

	op := func(account, body string) {
		createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,%s}`, account, body))
	}
	op(acc.ID, `"type":"deposit","occurred_on":"2026-07-01","amount_minor":150000,"currency":"USD"`)
	op(acc.ID, `"type":"deposit","occurred_on":"2026-07-01","amount_minor":500000,"currency":"RUB"`)
	op(acc.ID, fmt.Sprintf(`"instrument_id":%q,"type":"buy","occurred_on":"2026-07-10","quantity":"10","price":"100","currency":"USD"`, acme.ID))
	op(acc.ID, fmt.Sprintf(`"instrument_id":%q,"type":"buy","occurred_on":"2026-07-10","quantity":"5","price":"10","currency":"USD"`, beta.ID))
	op(acc.ID, fmt.Sprintf(`"instrument_id":%q,"type":"buy","occurred_on":"2026-07-10","quantity":"10","price":"300","currency":"RUB"`, sber.ID))
	// A position closed out adds nothing and is not counted as unpriced.
	op(acc.ID, fmt.Sprintf(`"instrument_id":%q,"type":"buy","occurred_on":"2026-07-10","quantity":"4","price":"40","currency":"RUB"`, gamma.ID))
	op(acc.ID, fmt.Sprintf(`"instrument_id":%q,"type":"sell","occurred_on":"2026-07-20","quantity":"4","price":"45","currency":"RUB"`, gamma.ID))
	// Bought with dollars nobody recorded arriving.
	op(short.ID, fmt.Sprintf(`"instrument_id":%q,"type":"buy","occurred_on":"2026-07-10","quantity":"1","price":"100","currency":"USD"`, acme.ID))

	var spaceID uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT id FROM spaces LIMIT 1`).Scan(&spaceID); err != nil {
		t.Fatalf("read the space: %v", err)
	}
	h := portfolio.NewHandler(operation.NewStore(pool), instrument.NewStore(pool), quotes, conv, family.NewStore(pool), nil, nil)

	got, err := h.ValueFromJournal(t.Context(), spaceID, uuid.MustParse(acc.ID))
	if err != nil {
		t.Fatalf("ValueFromJournal: %v", err)
	}
	// Dollars: ACME 10 × 120 = 1 200, cash 1 500 − 1 000 − 50 = 450, BETA
	// unpriced → 1 650 $ × 90 = 148 500 ₽. Rubles: SBER 10 × 310 = 3 100, cash
	// 5 000 − 3 000 − 160 + 180 = 2 020 → 5 120 ₽. In all 153 620 ₽.
	if got.Minor != 15_362_000 {
		t.Errorf("value = %d, want 15362000", got.Minor)
	}
	if want := map[string]int64{"USD": 165_000, "RUB": 512_000}; !maps.Equal(got.ByCurrency, want) {
		t.Errorf("by currency = %v, want %v", got.ByCurrency, want)
	}
	if got.Currency != "RUB" || got.Operations != 7 || got.Unpriced != 1 ||
		len(got.MissingRates) != 0 || len(got.NegativeCash) != 0 {
		t.Errorf("value = %+v, want RUB, 7 operations, 1 unpriced, nothing missing, no negative cash", got)
	}

	shortValue, err := h.ValueFromJournal(t.Context(), spaceID, uuid.MustParse(short.ID))
	if err != nil {
		t.Fatalf("ValueFromJournal: %v", err)
	}
	// 1 × 120 − 100 = 20 $ → 1 800 ₽, and the minus is said.
	if shortValue.Minor != 180_000 || !slices.Equal(shortValue.NegativeCash, []string{"USD"}) {
		t.Errorf("value = %+v, want 180000 with USD named as cash below zero", shortValue)
	}

	empty := createAccount(t, c, url, `{"name":"Пустой","type":"brokerage","currency":"RUB"}`)
	none, err := h.ValueFromJournal(t.Context(), spaceID, uuid.MustParse(empty.ID))
	if err != nil {
		t.Fatalf("ValueFromJournal: %v", err)
	}
	if none.Operations != 0 || none.Minor != 0 {
		t.Errorf("value of an account with no operations = %+v, want nothing to value it from", none)
	}
}

// A currency held with no rate into the base one today is left out of the
// figure and named, rather than counted at 1:1 or at nought without a word.
func TestACurrencyWithNoRateIsLeftOutAndNamed(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	conv := marketdata.NewConverter(mdStore)
	url, c := setupAPI(t, pool, quotes, conv)

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	createOperation(t, c, url, fmt.Sprintf(
		`{"account_id":%q,"type":"deposit","occurred_on":"2026-07-01","amount_minor":10000,"currency":"EUR"}`, acc.ID))
	createOperation(t, c, url, fmt.Sprintf(
		`{"account_id":%q,"type":"deposit","occurred_on":"2026-07-01","amount_minor":30000,"currency":"RUB"}`, acc.ID))

	var spaceID uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT id FROM spaces LIMIT 1`).Scan(&spaceID); err != nil {
		t.Fatalf("read the space: %v", err)
	}
	h := portfolio.NewHandler(operation.NewStore(pool), instrument.NewStore(pool), quotes, conv, family.NewStore(pool), nil, nil)
	got, err := h.ValueFromJournal(t.Context(), spaceID, uuid.MustParse(acc.ID))
	if err != nil {
		t.Fatalf("ValueFromJournal: %v", err)
	}
	if got.Minor != 30_000 || !slices.Equal(got.MissingRates, []string{"EUR"}) {
		t.Errorf("value = %+v, want the 300 ₽ alone with EUR named as having no rate", got)
	}
}
