package operation

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
)

var nvda = uuid.MustParse("00000000-0000-0000-0000-00000000a001")

func onDay(s string) time.Time {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return d
}

func buyNVDA(on string, quantity int64) Operation {
	q := decimal.NewFromInt(quantity)
	return Operation{
		Type: TypeBuy, InstrumentID: &nvda, OccurredOn: onDay(on), Quantity: &q,
		AmountMinor: -10_000 * quantity, Currency: "USD",
	}
}

func dividendNVDA(on string, minor int64, currency string) Operation {
	return Operation{
		ID: uuid.New(), Type: TypeDividend, InstrumentID: &nvda, OccurredOn: onDay(on),
		AmountMinor: minor, Currency: currency,
	}
}

func declared(record, paid string, perShare string) marketdata.Dividend {
	p := onDay(paid)
	return marketdata.Dividend{
		InstrumentID: nvda, RecordDate: onDay(record), PaymentDate: &p,
		PerShare: decimal.RequireFromString(perShare), Currency: "USD",
	}
}

// The estimate is per share × shares held less what arrived, at the rate it
// implies: 5 × 0,04 $ = 0,20 $ before withholding, 0,14 $ arrived, 0,06 $ = 30%
// taken (the owner's NVDA, 2021), and 3 × 0,04 $ with 0,11 $ arrived after
// a W-8BEN, ≈ 8,3%.
func TestTheWithheldTaxIsEstimatedFromTheCalendar(t *testing.T) {
	for name, tc := range map[string]struct {
		shares     int64
		received   int64
		gross, tax int64
		rate       string
	}{
		"30% (no W-8BEN)": {5, 14, 20, 6, "30.0"},
		"after a W-8BEN":  {3, 11, 12, 1, "8.3"},
	} {
		d := dividendNVDA("2021-09-29", tc.received, "USD")
		journal := []Operation{buyNVDA("2021-06-01", tc.shares), d}
		w, err := estimateWithheld(d, journal, []marketdata.Dividend{declared("2021-09-02", "2021-09-29", "0.04")})
		if err != nil {
			t.Fatal(err)
		}
		if w.State != apitypes.WithheldAbroadStateEstimated {
			t.Fatalf("%s: state = %s (%v), want estimated", name, w.State, w.UnknownReason)
		}
		if w.GrossMinor.MustGet() != tc.gross || w.TaxMinor.MustGet() != tc.tax || w.RatePercent.MustGet() != tc.rate {
			t.Errorf("%s: gross %d, tax %d, rate %s; want %d, %d, %s", name,
				w.GrossMinor.MustGet(), w.TaxMinor.MustGet(), w.RatePercent.MustGet(), tc.gross, tc.tax, tc.rate)
		}
		if w.Shares.MustGet() != decimal.NewFromInt(tc.shares).String() || w.RecordDate.MustGet() != "2021-09-02" {
			t.Errorf("%s: shares %s on %s, want %d on 2021-09-02", name, w.Shares.MustGet(), w.RecordDate.MustGet(), tc.shares)
		}
	}
}

// Shares bought after the last day that carried the right are not counted.
func TestTheWithheldTaxCountsOnlySharesThatCarriedTheRight(t *testing.T) {
	d := dividendNVDA("2021-09-29", 14, "USD")
	lastBuy := onDay("2021-09-01")
	dv := declared("2021-09-02", "2021-09-29", "0.04")
	dv.LastBuyDate = &lastBuy
	journal := []Operation{buyNVDA("2021-06-01", 5), buyNVDA("2021-09-02", 100), d}
	w, err := estimateWithheld(d, journal, []marketdata.Dividend{dv})
	if err != nil {
		t.Fatal(err)
	}
	if got := w.Shares.MustGet(); got != "5" {
		t.Errorf("shares = %s, want 5: the 100 bought on the record date carried no right", got)
	}
}

// A payment in parts is one dividend: every part counts toward what arrived.
func TestAPaymentInPartsIsOneDividend(t *testing.T) {
	first, second := dividendNVDA("2021-09-29", 10, "USD"), dividendNVDA("2021-09-30", 4, "USD")
	journal := []Operation{buyNVDA("2021-06-01", 5), first, second}
	w, err := estimateWithheld(first, journal, []marketdata.Dividend{declared("2021-09-02", "2021-09-29", "0.04")})
	if err != nil {
		t.Fatal(err)
	}
	if w.ReceivedMinor != 14 || w.TaxMinor.MustGet() != 6 {
		t.Errorf("received %d, tax %d; want 14 and 6", w.ReceivedMinor, w.TaxMinor.MustGet())
	}
}

// Paid whole with the tax as the broker's own row: the row is the answer.
func TestAPaymentPaidWholeLeavesTheTaxToTheBrokersRow(t *testing.T) {
	d := dividendNVDA("2021-09-29", 20, "USD")
	tax := Operation{Type: TypeTax, InstrumentID: &nvda, OccurredOn: onDay("2021-09-29"), AmountMinor: -2, Currency: "USD"}
	journal := []Operation{buyNVDA("2021-06-01", 5), d, tax}
	w, err := estimateWithheld(d, journal, []marketdata.Dividend{declared("2021-09-02", "2021-09-29", "0.04")})
	if err != nil {
		t.Fatal(err)
	}
	if w.State != apitypes.WithheldAbroadStateReported {
		t.Fatalf("state = %s, want reported", w.State)
	}
	if len(w.BrokerTax) != 1 || w.BrokerTax[0].AmountMinor != -2 {
		t.Errorf("broker tax = %+v, want the -2 row", w.BrokerTax)
	}
}

// Where no honest estimate exists the answer says why.
func TestTheWithheldTaxIsUnknownRatherThanGuessed(t *testing.T) {
	calendar := []marketdata.Dividend{declared("2021-09-02", "2021-09-29", "0.04")}
	for name, tc := range map[string]struct {
		journal  []Operation
		row      int
		calendar []marketdata.Dividend
		want     apitypes.WithheldAbroadUnknownReason
	}{
		"no calendar":     {[]Operation{buyNVDA("2021-06-01", 5), dividendNVDA("2021-09-29", 14, "USD")}, 1, nil, apitypes.NoCalendar},
		"not in it":       {[]Operation{buyNVDA("2021-06-01", 5), dividendNVDA("2022-06-29", 14, "USD")}, 1, calendar, apitypes.NotInCalendar},
		"paid in roubles": {[]Operation{buyNVDA("2021-06-01", 5), dividendNVDA("2021-09-29", 1100, "RUB")}, 1, calendar, apitypes.AnotherCurrency},
		"nothing held":    {[]Operation{dividendNVDA("2021-09-29", 14, "USD"), buyNVDA("2021-09-10", 5)}, 0, calendar, apitypes.NoHolding},
		"more than gross": {[]Operation{buyNVDA("2021-06-01", 5), dividendNVDA("2021-09-29", 50, "USD")}, 1, calendar, apitypes.Implausible},
		"over half of it": {[]Operation{buyNVDA("2021-06-01", 5), dividendNVDA("2021-09-29", 5, "USD")}, 1, calendar, apitypes.Implausible},
	} {
		w, err := estimateWithheld(tc.journal[tc.row], tc.journal, tc.calendar)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if w.State != apitypes.WithheldAbroadStateUnknown || w.UnknownReason.MustGet() != tc.want {
			t.Errorf("%s: state %s, reason %v; want unknown, %s", name, w.State, w.UnknownReason, tc.want)
		}
		if w.GrossMinor.IsSpecified() && !w.GrossMinor.IsNull() {
			t.Errorf("%s: a gross of %d published beside unknown", name, w.GrossMinor.MustGet())
		}
	}
}
