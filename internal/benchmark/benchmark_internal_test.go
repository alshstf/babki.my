package benchmark

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
)

func d(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

// The family had 100 000 at the start, put 50 000 in on 1 July and took 30 000
// out on 1 October. The index stood at 100, 125, 120 and 150 at the end: 1 000
// units, 400 more, 250 sold — 1 150 units, worth 172 500.
var familyBasis = account.ReturnBasis{
	Start: account.JournalValue{Minor: 100_000_00},
	Flows: []account.ReturnFlow{{Day: d("2026-07-01"), Minor: -50_000_00}, {Day: d("2026-10-01"), Minor: 30_000_00}},
	End:   account.JournalValue{Minor: 160_000_00},
}

var closes = map[string]map[string]string{
	"MCFTR":   {"2026-01-01": "100", "2026-07-01": "125", "2026-10-01": "120", "2026-12-31": "150"},
	"RGBITR":  {"2026-01-01": "100", "2026-07-01": "101", "2026-12-31": "104"},
	"SP500TR": {"2026-01-01": "10", "2026-07-01": "11", "2026-10-01": "12", "2026-12-31": "12"},
}

type fakeIndices struct{}

func (fakeIndices) IndexValueOn(_ context.Context, code string, on time.Time) (decimal.Decimal, bool, error) {
	v, ok := closes[code][on.Format(time.DateOnly)]
	if !ok {
		return decimal.Zero, false, nil
	}
	return decimal.RequireFromString(v), true, nil
}

type fakeBasis struct{}

func (fakeBasis) FamilyReturnBasis(context.Context, uuid.UUID, string, time.Time, time.Time) (account.ReturnBasis, error) {
	return familyBasis, nil
}

type fakeSpaces struct{}

func (fakeSpaces) SpaceByID(context.Context, uuid.UUID) (family.Space, error) {
	return family.Space{BaseCurrency: "RUB"}, nil
}

// fakeRates: the dollar at 90 roubles all year.
type fakeRates struct{}

func (fakeRates) Rate(_ context.Context, from, to string, _ time.Time) (decimal.Decimal, time.Time, error) {
	if from == "USD" && to == "RUB" {
		return decimal.NewFromInt(90), time.Time{}, nil
	}
	return decimal.Zero, time.Time{}, marketdata.ErrNoRate
}

func (fakeRates) RatesOn(context.Context, []marketdata.RateQuery) (marketdata.Rates, error) {
	return marketdata.Rates{}, nil
}

func TestTheSameMoneyGoesIntoTheIndex(t *testing.T) {
	svc := NewService(fakeBasis{}, fakeSpaces{}, fakeIndices{}, fakeRates{})
	base, got, err := svc.Compare(context.Background(), uuid.New(), d("2026-01-01"), d("2026-12-31"))
	if err != nil {
		t.Fatal(err)
	}
	if base != "RUB" || len(got) != 3 {
		t.Fatalf("results = %s %+v", base, got)
	}
	mcftr, rgbitr, sp := got[0], got[1], got[2]
	if !mcftr.Complete || mcftr.End != 172_500_00 || mcftr.AnnualRate == nil {
		t.Fatalf("MCFTR = %+v", mcftr)
	}
	// The index made more than the family's 160 000: its rate is above the
	// family's, both counted on the same flows.
	if *mcftr.AnnualRate < 0.40 || *mcftr.AnnualRate > 0.60 {
		t.Errorf("MCFTR rate = %v", *mcftr.AnnualRate)
	}
	// RGBITR has no close on 1 October: the model cannot be priced.
	if rgbitr.Complete || rgbitr.AnnualRate != nil {
		t.Errorf("RGBITR = %+v, want incomplete", rgbitr)
	}
	// The S&P 500 in dollars at 90: 100 000 buys 111.1… units at 900 roubles,
	// 50 000 more at 990, 30 000 sold at 1 080 — 134.4… units at 1 080.
	units := 100_000.0/900 + 50_000.0/990 - 30_000.0/1080
	if want := units * 1080; !sp.Complete || math.Abs(float64(sp.End)/100-want) > 0.02 {
		t.Errorf("S&P 500 = %+v, want about %.2f", sp, want)
	}
}

// Taking out more than the model holds makes it owe the index: no figure.
func TestAModelThatWouldOweTheIndexIsNotCompared(t *testing.T) {
	b := account.ReturnBasis{
		Start: account.JournalValue{Minor: 10_000_00},
		Flows: []account.ReturnFlow{{Day: d("2026-07-01"), Minor: 50_000_00}},
		End:   account.JournalValue{Minor: 1_00},
	}
	price := func(on time.Time) (decimal.Decimal, bool, error) {
		return decimal.RequireFromString(closes["MCFTR"][on.Format(time.DateOnly)]), true, nil
	}
	r, err := model(marketdata.Benchmarks[0], b, d("2026-01-01"), d("2026-12-31"), price)
	if err != nil {
		t.Fatal(err)
	}
	if r.Complete || r.AnnualRate != nil {
		t.Errorf("result = %+v, want no figure", r)
	}
}
