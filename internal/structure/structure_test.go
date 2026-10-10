package structure_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/structure"
)

type fakeAccounts []account.WithBalance

func (f fakeAccounts) ListWithBalance(context.Context, uuid.UUID) ([]account.WithBalance, error) {
	return f, nil
}

type fakePositions map[uuid.UUID]apitypes.PositionsResponse

func (f fakePositions) Positions(_ context.Context, _, id uuid.UUID) (apitypes.PositionsResponse, int, error) {
	r, ok := f[id]
	if !ok {
		return apitypes.PositionsResponse{}, 0, nil
	}
	return r, 5, nil
}

type fakeSpaces struct{}

func (fakeSpaces) SpaceByID(context.Context, uuid.UUID) (family.Space, error) {
	return family.Space{BaseCurrency: "RUB"}, nil
}

// fakeRates knows the dollar at 90 roubles and nothing else.
type fakeRates struct{}

func (fakeRates) Rate(_ context.Context, from, to string, _ time.Time) (decimal.Decimal, time.Time, error) {
	if from == "USD" && to == "RUB" {
		return decimal.NewFromInt(90), time.Now(), nil
	}
	return decimal.Zero, time.Time{}, marketdata.ErrNoRate
}

func (fakeRates) RatesOn(context.Context, []marketdata.RateQuery) (marketdata.Rates, error) {
	return marketdata.Rates{}, nil
}

func position(typ apitypes.InstrumentType, isin, currency string, liquid, full *int64) apitypes.Position {
	p := apitypes.Position{
		Instrument: apitypes.Instrument{Type: typ, Isin: isin}, Currency: currency, Quantity: "1",
		LiquidValueMinor: nullable.NewNullNullable[int64](), MarketValueMinor: nullable.NewNullNullable[int64](),
		MarketValueCurrency: nullable.NewNullableWithValue(currency),
	}
	if liquid != nil {
		p.LiquidValueMinor = nullable.NewNullableWithValue(*liquid)
	}
	if full != nil {
		p.MarketValueMinor = nullable.NewNullableWithValue(*full)
	}
	return p
}

func of(v int64) *int64 { return &v }

// A broker's journal comes apart into shares, bonds, funds and its cash, by
// issuer country; a card counted by its balance is money; a credit card is a
// debt; a dollar holding counts at today's rate; a frozen fund counts only in
// the full valuation.
func TestTheWorthComesApart(t *testing.T) {
	broker, card, credit, idle := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	accounts := fakeAccounts{
		{Account: account.Account{ID: broker, Type: account.TypeBrokerage, Currency: "RUB", Status: account.StatusActive}},
		{
			Account: account.Account{ID: card, Type: account.TypeChecking, Currency: "RUB", Status: account.StatusActive},
			Balance: &account.BalancePoint{AmountMinor: 100_000_00},
		},
		{
			Account: account.Account{ID: credit, Type: account.TypeCreditCard, Currency: "RUB", Status: account.StatusActive},
			Balance: &account.BalancePoint{AmountMinor: -30_000_00},
		},
		{
			Account: account.Account{ID: idle, Type: account.TypeDeposit, Currency: "RUB", Status: account.StatusArchived},
			Balance: &account.BalancePoint{AmountMinor: 1_000_000_00},
		},
	}
	positions := fakePositions{broker: {
		Positions: []apitypes.Position{
			position(apitypes.InstrumentTypeShare, "RU0009029540", "RUB", of(300_000_00), of(300_000_00)),
			position(apitypes.InstrumentTypeShare, "US0378331005", "USD", of(1_000_00), of(1_000_00)),
			position(apitypes.InstrumentTypeBond, "RU000A1038V6", "RUB", of(100_000_00), of(100_000_00)),
			position(apitypes.InstrumentTypeEtf, "RU000A0JR282", "USD", nil, of(500_00)),
		},
		Cash: []apitypes.CashPosition{{Currency: "RUB", AmountMinor: 20_000_00}},
	}}
	svc := structure.NewService(accounts, positions, fakeSpaces{}, fakeRates{})

	s, err := svc.Of(t.Context(), uuid.New(), structure.Liquid)
	if err != nil {
		t.Fatal(err)
	}
	// 300 000 + $1 000 (90 000) + 100 000 + 20 000 on the broker, 100 000 on the card.
	if s.Assets != 610_000_00 || s.Debts != -30_000_00 || s.Unpriced != 1 {
		t.Fatalf("assets %d, debts %d, unpriced %d", s.Assets, s.Debts, s.Unpriced)
	}
	want := map[string]int64{"shares": 390_000_00, "bonds": 100_000_00, "broker_cash": 20_000_00, "money": 100_000_00}
	for _, sl := range s.ByClass {
		if want[sl.Key] != sl.Minor {
			t.Errorf("class %s = %d, want %d", sl.Key, sl.Minor, want[sl.Key])
		}
	}
	if s.ByClass[0].Key != "shares" {
		t.Errorf("largest first: %v", s.ByClass)
	}
	countries := map[string]int64{}
	for _, sl := range s.ByCountry {
		countries[sl.Key] = sl.Minor
	}
	if countries["RU"] != 400_000_00 || countries["US"] != 90_000_00 || countries[""] != 120_000_00 {
		t.Errorf("countries = %v", countries)
	}
	currencies := map[string]int64{}
	for _, sl := range s.ByCurrency {
		currencies[sl.Key] = sl.Minor
	}
	if currencies["USD"] != 90_000_00 || currencies["RUB"] != 520_000_00 {
		t.Errorf("currencies = %v", currencies)
	}

	full, err := svc.Of(t.Context(), uuid.New(), structure.Full)
	if err != nil {
		t.Fatal(err)
	}
	if full.Assets != 610_000_00+45_000_00 || full.Unpriced != 0 {
		t.Errorf("full = %d, unpriced %d; want the frozen fund at $500 too", full.Assets, full.Unpriced)
	}
	if _, err := svc.Of(t.Context(), uuid.New(), "market"); err == nil {
		t.Error("an unknown valuation was accepted")
	}
}
