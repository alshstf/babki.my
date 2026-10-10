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

// Ten bonds, five more bought on the August coupon's record day — too late
// for it. August's coupon came, net of tax; September's never did; October's
// is days old and awaited. The dividend came at half: short. A paper sold
// before its coupon's record day is not due anything.
func TestPayoutsDueAreCheckedAgainstTheJournal(t *testing.T) {
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
	ofz, ko, gone := mk(instrument.TypeBond, "OFZ", "RUB"), mk(instrument.TypeShare, "KO", "USD"), mk(instrument.TypeBond, "GONE", "RUB")
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
	write(operation.Operation{InstrumentID: &ofz, Type: operation.TypeBuy, OccurredOn: day(t, "2026-08-04"), Quantity: &q5, Price: &p1000, AmountMinor: -500_000, Currency: "RUB"})
	write(operation.Operation{InstrumentID: &ko, Type: operation.TypeBuy, OccurredOn: day(t, "2026-02-01"), Quantity: &q20, Price: &p60, AmountMinor: -120_000, Currency: "USD"})
	write(operation.Operation{InstrumentID: &gone, Type: operation.TypeBuy, OccurredOn: day(t, "2026-02-01"), Quantity: &q5, Price: &p100, AmountMinor: -50_000, Currency: "RUB"})
	write(operation.Operation{InstrumentID: &gone, Type: operation.TypeSell, OccurredOn: day(t, "2026-03-01"), Quantity: &q5, Price: &p100, AmountMinor: 50_000, Currency: "RUB"})
	write(operation.Operation{InstrumentID: &ofz, Type: operation.TypeCoupon, OccurredOn: day(t, "2026-08-06"), AmountMinor: 30_800, Currency: "RUB"})
	write(operation.Operation{InstrumentID: &ko, Type: operation.TypeDividend, OccurredOn: day(t, "2026-09-03"), AmountMinor: 500, Currency: "USD"})

	coupon := dec("35.4")
	if err := md.ReplaceBondEvents(ctx, ofz, "moex", []marketdata.BondEvent{
		{InstrumentID: ofz, Source: "moex", Kind: marketdata.BondCoupon, On: day(t, "2026-08-05"), Value: &coupon, Currency: "RUB"},
		{InstrumentID: ofz, Source: "moex", Kind: marketdata.BondCoupon, On: day(t, "2026-09-02"), Value: &coupon, Currency: "RUB"},
		{InstrumentID: ofz, Source: "moex", Kind: marketdata.BondCoupon, On: day(t, "2026-10-07"), Value: &coupon, Currency: "RUB"},
		{InstrumentID: ofz, Source: "moex", Kind: marketdata.BondCoupon, On: day(t, "2026-11-04"), Value: &coupon, Currency: "RUB"},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := md.ReplaceBondEvents(ctx, gone, "moex", []marketdata.BondEvent{
		{InstrumentID: gone, Source: "moex", Kind: marketdata.BondCoupon, On: day(t, "2026-08-01"), Value: &coupon, Currency: "RUB"},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	pay := day(t, "2026-09-01")
	if err := md.ReplaceDividends(ctx, ko, "tinvest", []marketdata.Dividend{
		{InstrumentID: ko, Source: "tinvest", RecordDate: day(t, "2026-08-20"), PaymentDate: &pay, PerShare: dec("0.51"), Currency: "USD"},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	svc := NewService(opStore, accStore, md, fam, marketdata.NewConverter(md))
	svc.now = func() time.Time { return day(t, "2026-10-10") }
	checks, err := svc.Received(ctx, sp.ID, 90, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		on     string
		kind   Kind
		qty    string
		amount int64
		status Status
		got    int64
	}
	var got []row
	for _, c := range checks {
		r := row{on: c.On.Format(time.DateOnly), kind: c.Kind, qty: c.Quantity.String(), status: c.Status, got: c.Got}
		if c.Amount != nil {
			r.amount = *c.Amount
		}
		got = append(got, r)
	}
	want := []row{
		{"2026-10-07", KindCoupon, "15", 53_100, Awaited, 0},
		{"2026-09-02", KindCoupon, "15", 53_100, Missing, 0},
		{"2026-09-01", KindDividend, "20", 1_020, Short, 500},
		{"2026-08-05", KindCoupon, "10", 35_400, Received, 30_800},
	}
	if len(got) != len(want) {
		t.Fatalf("checks = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("check %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if _, err := svc.Received(ctx, sp.ID, MaxDays+1, Scope{}); err == nil {
		t.Error("a check past a year back was accepted")
	}
}
