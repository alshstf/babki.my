package operation_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

// withheldItem decodes a journal row's withheld_abroad.
type withheldItem struct {
	ID             string `json:"id"`
	WithheldAbroad *struct {
		State         string  `json:"state"`
		UnknownReason *string `json:"unknown_reason"`
		ReceivedMinor int64   `json:"received_minor"`
		TaxMinor      *int64  `json:"tax_minor"`
		RatePercent   *string `json:"rate_percent"`
		TaxInBase     *struct {
			Currency    string `json:"currency"`
			AmountMinor int64  `json:"amount_minor"`
		} `json:"tax_in_base"`
		BrokerTax []struct {
			Currency    string `json:"currency"`
			AmountMinor int64  `json:"amount_minor"`
		} `json:"broker_tax"`
	} `json:"withheld_abroad"`
}

// The journal publishes the estimate on a foreign dividend, with the tax in
// the base currency at the payment day's rate, and nothing on a Russian one.
func TestTheJournalEstimatesTheTaxWithheldAbroad(t *testing.T) {
	url, c, mdStore := newAPIWithConverter(t)
	seedFxRate(t, mdStore, "2021-09-29", "73.00")

	acc := mkAccount(t, url, c, "Брокер", "USD")
	nvda := mkInstrument(t, url, c, `{"type":"share","name":"NVIDIA","ticker":"NVDA","isin":"US67066G1040","currency":"USD"}`)
	sber := mkInstrument(t, url, c, `{"type":"share","name":"Сбербанк","ticker":"SBER","isin":"RU0009029540","currency":"USD"}`)
	paid := time.Date(2021, 9, 29, 0, 0, 0, 0, time.UTC)
	if err := mdStore.ReplaceDividends(t.Context(), uuid.MustParse(nvda), "test", []marketdata.Dividend{{
		InstrumentID: uuid.MustParse(nvda), Source: "test", RecordDate: time.Date(2021, 9, 2, 0, 0, 0, 0, time.UTC),
		PaymentDate: &paid, PerShare: decimal.RequireFromString("0.04"), Currency: "USD",
	}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, paper := range []string{nvda, sber} {
		mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy","occurred_on":"2021-06-01",
			"quantity":"5","price":"100","amount_minor":-50000,"currency":"USD"}`, acc, paper))
	}
	foreign := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":"2021-09-29","amount_minor":14,"currency":"USD"}`, acc, nvda))
	russian := mkOperation(t, url, c, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"dividend",
		"occurred_on":"2021-09-29","amount_minor":14,"currency":"USD"}`, acc, sber))

	resp := do(t, c, "GET", url+"/api/v1/accounts/"+acc+"/operations", "")
	var page struct {
		Operations []withheldItem `json:"operations"`
	}
	decodeJSON(t, resp, &page)
	rows := map[string]withheldItem{}
	for _, o := range page.Operations {
		rows[o.ID] = o
	}

	w := rows[foreign].WithheldAbroad
	if w == nil || w.State != "estimated" || w.TaxMinor == nil || *w.TaxMinor != 6 || *w.RatePercent != "30.0" {
		t.Fatalf("foreign dividend: withheld_abroad = %+v, want estimated 6 at 30.0", w)
	}
	// 0,06 $ at 73 ₽.
	if w.TaxInBase == nil || w.TaxInBase.Currency != "RUB" || w.TaxInBase.AmountMinor != 438 {
		t.Errorf("tax_in_base = %+v, want 438 RUB", w.TaxInBase)
	}
	if rows[russian].WithheldAbroad != nil {
		t.Errorf("a Russian paper's dividend carries withheld_abroad = %+v, want null", rows[russian].WithheldAbroad)
	}
}
