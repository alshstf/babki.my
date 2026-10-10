package moex

import (
	"testing"

	"babki.my/babki/internal/marketdata"
)

// A bondization answer read: a set coupon and one not yet set, a partial
// repayment, the redemption the exchange marks «maturity», an offer; a row
// without a currency is left out.
func TestAScheduleIsReadFromTheExchangesTables(t *testing.T) {
	coupons := issTable{
		Columns: []string{"coupondate", "recorddate", "startdate", "faceunit", "value", "valueprc"},
		Data: [][]any{
			{"2026-11-18", "2026-11-17", "2026-05-20", "SUR", 35.4, 7.1},
			{"2027-05-19", "2027-05-18", "2026-11-18", "SUR", nil, nil},
			{"2027-11-17", nil, nil, nil, 1.0, nil},
		},
	}
	amortizations := issTable{
		Columns: []string{"amortdate", "faceunit", "value", "valueprc", "data_source"},
		Data: [][]any{
			{"2030-05-15", "RUB", 300.0, 30.0, "amortization"},
			{"2041-05-15", "RUB", 700.0, 70.0, "maturity"},
		},
	}
	offers := issTable{
		Columns: []string{"offerdate", "faceunit", "price", "value"},
		Data:    [][]any{{"2028-02-01", "RUB", 100.0, 1000.0}},
	}
	events, err := scheduleEvents(coupons, amortizations, offers)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 {
		t.Fatalf("events = %+v, want five", events)
	}
	first := events[0]
	if first.Kind != marketdata.BondCoupon || first.Currency != "RUB" || first.Value.String() != "35.4" ||
		first.RecordOn == nil || first.RecordOn.Format("2006-01-02") != "2026-11-17" || first.Percent.String() != "7.1" {
		t.Errorf("the set coupon = %+v", first)
	}
	if events[1].Value != nil || events[1].Percent != nil {
		t.Errorf("the coupon not yet set = %+v, want no value", events[1])
	}
	if events[2].Kind != marketdata.BondAmortization || events[3].Kind != marketdata.BondRedemption || events[3].Value.String() != "700" {
		t.Errorf("repayments = %+v, %+v", events[2], events[3])
	}
	if events[4].Kind != marketdata.BondOffer || events[4].Percent.String() != "100" {
		t.Errorf("the offer = %+v", events[4])
	}
}
