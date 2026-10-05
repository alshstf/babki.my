package portfolio_test

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/testdb"
)

// positionsByTicker fetches an account's positions keyed by ticker.
func positionsByTicker(t *testing.T, c *http.Client, url, accountID string) map[string]positionResp {
	t.Helper()
	resp := do(t, c, "GET", url+"/api/v1/accounts/"+accountID+"/positions", "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET positions = %d, want 200: %s", resp.StatusCode, b)
	}
	var got positionsResp
	decodeJSON(t, resp, &got)
	out := make(map[string]positionResp, len(got.Positions))
	for _, p := range got.Positions {
		out[p.Instrument.Ticker] = p
	}
	return out
}

// has_undated_lots tells a reader why in_base is null: an undated lot never
// resolves, a missing rate does. Three positions in one response:
//
//	ACME  — transfer with its breakdown dropped: true, in_base null
//	BETA  — dated buy with a rate: false, in_base present
//	GAMMA — dated buy before every rate: false, in_base null
func TestPositionSaysWhenALotHasNoAcquisitionDate(t *testing.T) {
	pool := testdb.New(t)
	mdStore := marketdata.NewStore(pool)
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := setupAPI(t, pool, quotes, marketdata.NewConverter(mdStore))
	// The table starts on earlyRateOn (2026-02-01), so a buy dated before it
	// resolves no rate at all while every later date resolves fine.
	if err := mdStore.UpsertFxRates(t.Context(), []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: mustDate(t, earlyRateOn), Rate: decimal.RequireFromString("60"), Source: "test"},
		{Base: "USD", Quote: "RUB", On: mustDate(t, lateRateOn), Rate: decimal.RequireFromString("90"), Source: "test"},
	}); err != nil {
		t.Fatalf("seed fx rates: %v", err)
	}

	from := createAccount(t, c, url, `{"name":"Старый брокер","type":"brokerage","currency":"USD"}`)
	to := createAccount(t, c, url, `{"name":"Новый брокер","type":"brokerage","currency":"USD"}`)
	acme := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"USD"}`)
	beta := createInstrument(t, c, url, `{"type":"share","name":"Бета","ticker":"BETA","currency":"USD"}`)
	gamma := createInstrument(t, c, url, `{"type":"share","name":"Гамма","ticker":"GAMMA","currency":"USD"}`)

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"USD"}`, from.ID, acme.ID, earlyBuyOn))
	createTransfer(t, c, url, fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"instrument_id":%q,
		"quantity":"10","occurred_on":%q}`, from.ID, to.ID, acme.ID, transferOn))
	// Turn it into a transfer recorded before breakdowns were kept: the basis
	// survives on the operation, the dates behind it do not.
	if _, err := pool.Exec(t.Context(), `DELETE FROM operation_transfer_lots`); err != nil {
		t.Fatalf("drop the stored breakdown: %v", err)
	}

	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":%q,"quantity":"10","price":"200",
		"amount_minor":-200000,"currency":"USD"}`, to.ID, beta.ID, lateBuyOn))
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-01-05","quantity":"10","price":"300",
		"amount_minor":-300000,"currency":"USD"}`, to.ID, gamma.ID))

	got := positionsByTicker(t, c, url, to.ID)
	if len(got) != 3 {
		t.Fatalf("positions = %+v, want ACME, BETA and GAMMA", got)
	}

	if !got["ACME"].HasUndatedLots {
		t.Errorf("ACME.has_undated_lots = false, want true: its only lot arrived by a transfer with no stored breakdown, so nothing knows when those shares were bought")
	}
	if got["ACME"].InBase != nil {
		t.Errorf("ACME.in_base = %+v, want null alongside has_undated_lots", *got["ACME"].InBase)
	}

	if got["BETA"].HasUndatedLots {
		t.Errorf("BETA.has_undated_lots = true, want false: an ordinary buy always knows its own date")
	}
	if got["BETA"].InBase == nil {
		t.Error("BETA.in_base = null, want a figure: its lot is dated and its date has a rate — the fixture is not testing what it means to")
	}

	if got["GAMMA"].HasUndatedLots {
		t.Errorf("GAMMA.has_undated_lots = true, want false: its lot is dated 2026-01-05 and merely has no fx rate that far back. Reporting an undated lot here would tell the reader a permanent, unrecoverable gap where the fx backfill job will in fact fill the number in")
	}
	if got["GAMMA"].InBase != nil {
		t.Errorf("GAMMA.in_base = %+v, want null: no rate exists on or before 2026-01-05", *got["GAMMA"].InBase)
	}
}
