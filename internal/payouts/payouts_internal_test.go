package payouts

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/testdb"
)

func day(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Ten bonds pay their coupon and the second, not yet set, is listed without an
// amount; an offer pays nothing by itself; a declared dollar dividend on 20
// shares comes in at today's rate; a payout past the window or before today is
// not there; a sold-out paper pays nothing.
func TestTheForecastCarriesTodaysHoldingsForward(t *testing.T) {
	pool := testdb.New(t)
	ctx := t.Context()
	fam := family.NewStore(pool)
	u, err := fam.CreateUser(ctx, "alex", "A", "h")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := fam.CreateSpaceWithOwner(ctx, "S", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	accStore, papers, md := account.NewStore(pool), instrument.NewStore(pool), marketdata.NewStore(pool)
	opStore := operation.NewStore(pool)
	ops := operation.NewService(opStore)
	broker, err := accStore.Create(ctx, sp.ID, nil, "Брокер", account.TypeBrokerage, "RUB", "")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(typ instrument.Type, ticker, currency string) uuid.UUID {
		p, err := papers.Create(ctx, instrument.Instrument{Type: typ, Name: ticker, Ticker: ticker, Currency: currency})
		if err != nil {
			t.Fatal(err)
		}
		return p.ID
	}
	ofz := mk(instrument.TypeBond, "OFZ", "RUB")
	ko := mk(instrument.TypeShare, "KO", "USD")
	gone := mk(instrument.TypeBond, "GONE", "RUB")
	dec := decimal.RequireFromString
	write := func(op operation.Operation) {
		op.AccountID = broker.ID
		if _, err := ops.Create(ctx, sp.ID, op); err != nil {
			t.Fatalf("%s: %v", op.Type, err)
		}
	}
	q10, q20, q5 := dec("10"), dec("20"), dec("5")
	p1000, p60, p100 := dec("1000"), dec("60"), dec("100")
	write(operation.Operation{Type: operation.TypeDeposit, OccurredOn: day(t, "2026-01-10"), AmountMinor: 10_000_000, Currency: "RUB"})
	write(operation.Operation{Type: operation.TypeDeposit, OccurredOn: day(t, "2026-01-10"), AmountMinor: 200_000, Currency: "USD"})
	write(operation.Operation{InstrumentID: &ofz, Type: operation.TypeBuy, OccurredOn: day(t, "2026-02-01"), Quantity: &q10, Price: &p1000, AmountMinor: -1_000_000, Currency: "RUB"})
	write(operation.Operation{InstrumentID: &ko, Type: operation.TypeBuy, OccurredOn: day(t, "2026-02-01"), Quantity: &q20, Price: &p60, AmountMinor: -120_000, Currency: "USD"})
	write(operation.Operation{InstrumentID: &gone, Type: operation.TypeBuy, OccurredOn: day(t, "2026-02-01"), Quantity: &q5, Price: &p100, AmountMinor: -50_000, Currency: "RUB"})
	write(operation.Operation{InstrumentID: &gone, Type: operation.TypeSell, OccurredOn: day(t, "2026-03-01"), Quantity: &q5, Price: &p100, AmountMinor: 50_000, Currency: "RUB"})

	coupon := dec("35.4")
	if err := md.ReplaceBondEvents(ctx, ofz, "moex", []marketdata.BondEvent{
		{InstrumentID: ofz, Source: "moex", Kind: marketdata.BondCoupon, On: day(t, "2026-09-02"), Value: &coupon, Currency: "RUB"},
		{InstrumentID: ofz, Source: "moex", Kind: marketdata.BondCoupon, On: day(t, "2026-11-18"), Value: &coupon, Currency: "RUB"},
		{InstrumentID: ofz, Source: "moex", Kind: marketdata.BondOffer, On: day(t, "2026-12-01"), Value: &p1000, Currency: "RUB"},
		{InstrumentID: ofz, Source: "moex", Kind: marketdata.BondCoupon, On: day(t, "2027-05-19"), Currency: "RUB"},
		{InstrumentID: ofz, Source: "moex", Kind: marketdata.BondCoupon, On: day(t, "2028-05-19"), Value: &coupon, Currency: "RUB"},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := md.ReplaceBondEvents(ctx, gone, "moex", []marketdata.BondEvent{
		{InstrumentID: gone, Source: "moex", Kind: marketdata.BondCoupon, On: day(t, "2026-11-01"), Value: &coupon, Currency: "RUB"},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	pay := day(t, "2026-12-15")
	if err := md.ReplaceDividends(ctx, ko, "tinvest", []marketdata.Dividend{
		{InstrumentID: ko, Source: "tinvest", RecordDate: day(t, "2026-11-28"), PaymentDate: &pay, PerShare: dec("0.51"), Currency: "USD"},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := md.UpsertFxRates(ctx, []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: day(t, "2026-10-09"), Rate: dec("90"), Source: "test"},
	}); err != nil {
		t.Fatal(err)
	}

	svc := NewService(opStore, accStore, md, fam, marketdata.NewConverter(md))
	svc.now = func() time.Time { return day(t, "2026-10-10") }
	f, err := svc.Forecast(ctx, sp.ID, 12, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Months) != 12 || f.Months[0].Format("2006-01") != "2026-10" || f.To.Format(time.DateOnly) != "2027-09-30" {
		t.Fatalf("window = %v … %v", f.Months[0], f.To)
	}
	type row struct {
		kind   Kind
		on     string
		amount int64
		known  bool
	}
	var got []row
	for _, e := range f.Events {
		r := row{kind: e.Kind, on: e.On.Format(time.DateOnly)}
		if e.Amount != nil {
			r.amount, r.known = *e.Amount, true
		}
		got = append(got, r)
	}
	want := []row{
		{KindCoupon, "2026-11-18", 35_400, true},
		{KindOffer, "2026-12-01", 0, false},
		{KindDividend, "2026-12-15", 1_020, true},
		{KindCoupon, "2027-05-19", 0, false},
	}
	if len(got) != len(want) {
		t.Fatalf("events = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// 354 ₽ + $10.20 at 90 = 918 ₽.
	if f.Total != 35_400+91_800 || f.ByMonth[1] != 35_400 || f.ByMonth[2] != 91_800 {
		t.Errorf("total %d, by month %v", f.Total, f.ByMonth)
	}
	if _, err := svc.Forecast(ctx, sp.ID, 25, nil); err == nil {
		t.Error("a 25-month forecast was accepted")
	}
}
