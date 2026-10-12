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

// What reaches the account (decision Р-38): the OFZ's August coupon came with
// its tax row, so November's comes the same, less 13 %; a broker that
// withholds at the year's end paid the YE bond's coupon whole, so it comes
// whole again; a rouble bond with no payout yet — less 13 % by the rule, its
// repayment of face value whole; a dollar dividend — before tax, no tax
// known.
func TestWhatReachesTheAccount(t *testing.T) {
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
	ofz, ye, fresh, ko := mk(instrument.TypeBond, "OFZ", "RUB"), mk(instrument.TypeBond, "YE", "RUB"), mk(instrument.TypeBond, "NEW", "RUB"), mk(instrument.TypeShare, "KO", "USD")
	dec := decimal.RequireFromString
	write := func(op operation.Operation) {
		op.AccountID = broker.ID
		if _, err := ops.Create(ctx, sp.ID, op); err != nil {
			t.Fatalf("%s: %v", op.Type, err)
		}
	}
	q10, q20, p1000, p60 := dec("10"), dec("20"), dec("1000"), dec("60")
	write(operation.Operation{Type: operation.TypeDeposit, OccurredOn: day(t, "2026-01-10"), AmountMinor: 10_000_000, Currency: "RUB"})
	write(operation.Operation{Type: operation.TypeDeposit, OccurredOn: day(t, "2026-01-10"), AmountMinor: 200_000, Currency: "USD"})
	for _, id := range []uuid.UUID{ofz, ye, fresh} {
		write(operation.Operation{InstrumentID: &id, Type: operation.TypeBuy, OccurredOn: day(t, "2026-02-01"), Quantity: &q10, Price: &p1000, AmountMinor: -1_000_000, Currency: "RUB"})
	}
	write(operation.Operation{InstrumentID: &ko, Type: operation.TypeBuy, OccurredOn: day(t, "2026-02-01"), Quantity: &q20, Price: &p60, AmountMinor: -120_000, Currency: "USD"})
	write(operation.Operation{InstrumentID: &ofz, Type: operation.TypeCoupon, OccurredOn: day(t, "2026-08-06"), AmountMinor: 35_400, Currency: "RUB"})
	write(operation.Operation{InstrumentID: &ofz, Type: operation.TypeTax, OccurredOn: day(t, "2026-08-06"), AmountMinor: -4_602, Currency: "RUB"})
	write(operation.Operation{InstrumentID: &ye, Type: operation.TypeCoupon, OccurredOn: day(t, "2026-08-06"), AmountMinor: 20_000, Currency: "RUB"})

	coupon, ycoupon, part := dec("35.4"), dec("20"), dec("100")
	events := func(id uuid.UUID, list ...marketdata.BondEvent) {
		for i := range list {
			list[i].InstrumentID, list[i].Source = id, "moex"
		}
		if err := md.ReplaceBondEvents(ctx, id, "moex", list, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	events(ofz,
		marketdata.BondEvent{Kind: marketdata.BondCoupon, On: day(t, "2026-08-05"), Value: &coupon, Currency: "RUB"},
		marketdata.BondEvent{Kind: marketdata.BondCoupon, On: day(t, "2026-11-04"), Value: &coupon, Currency: "RUB"})
	events(ye,
		marketdata.BondEvent{Kind: marketdata.BondCoupon, On: day(t, "2026-08-05"), Value: &ycoupon, Currency: "RUB"},
		marketdata.BondEvent{Kind: marketdata.BondCoupon, On: day(t, "2026-11-05"), Value: &ycoupon, Currency: "RUB"})
	events(fresh,
		marketdata.BondEvent{Kind: marketdata.BondCoupon, On: day(t, "2026-11-10"), Value: &coupon, Currency: "RUB"},
		marketdata.BondEvent{Kind: marketdata.BondAmortization, On: day(t, "2026-11-10"), Value: &part, Currency: "RUB"})
	pay := day(t, "2026-12-01")
	if err := md.ReplaceDividends(ctx, ko, "tinvest", []marketdata.Dividend{
		{InstrumentID: ko, Source: "tinvest", RecordDate: day(t, "2026-11-20"), PaymentDate: &pay, PerShare: dec("0.51"), Currency: "USD"},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	svc := NewService(opStore, accStore, md, fam, marketdata.NewConverter(md))
	svc.now = func() time.Time { return day(t, "2026-10-10") }
	checks, err := svc.Received(ctx, sp.ID, 90, Scope{InstrumentID: &ofz})
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 || checks[0].Got != 35_400 || checks[0].Tax != -4_602 || checks[0].Status != Received {
		t.Fatalf("OFZ's August coupon = %+v, want 35 400 with a tax of 4 602", checks)
	}

	f, err := svc.Forecast(ctx, sp.ID, 3, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		paper uuid.UUID
		kind  Kind
		gross int64
		net   int64
		by    NetBy
	}
	got := map[string]row{}
	for _, e := range f.Events {
		if e.Amount == nil {
			continue
		}
		got[e.On.Format(time.DateOnly)+" "+string(e.Kind)] = row{e.InstrumentID, e.Kind, *e.Amount, *e.Net, e.NetBy}
	}
	want := map[string]row{
		"2026-11-04 coupon":       {ofz, KindCoupon, 35_400, 30_798, NetByHistory},
		"2026-11-05 coupon":       {ye, KindCoupon, 20_000, 20_000, NetByHistory},
		"2026-11-10 coupon":       {fresh, KindCoupon, 35_400, 30_798, NetByRule},
		"2026-11-10 amortization": {fresh, KindAmortization, 100_000, 100_000, NetByRule},
		"2026-12-01 dividend":     {ko, KindDividend, 1_020, 1_020, NetByNone},
	}
	if len(got) != len(want) {
		t.Fatalf("payouts = %+v, want %+v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %+v, want %+v", k, got[k], w)
		}
	}
	if f.NetTotal >= f.Total {
		t.Errorf("net total %d is not below the total %d", f.NetTotal, f.Total)
	}
}
