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
	"babki.my/babki/internal/instrument"
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

// fakeRates knows the dollar at 90 roubles, a gram of gold at 10 000, and
// nothing else.
type fakeRates struct{}

func (fakeRates) Rate(_ context.Context, from, to string, _ time.Time) (decimal.Decimal, time.Time, error) {
	if from == "USD" && to == "RUB" {
		return decimal.NewFromInt(90), time.Now(), nil
	}
	if from == "XAU" && to == "RUB" {
		return decimal.NewFromInt(10_000), time.Now(), nil
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
	// In, yet not to be sold now (a rebalance keeps it, Р-37 В); at the liquid
	// valuation it is not in at all.
	if len(full.Unsellable) != 1 || full.Unsellable[0].Key != "funds" || full.Unsellable[0].Minor != 45_000_00 || len(s.Unsellable) != 0 {
		t.Errorf("unsellable: full %+v, liquid %+v", full.Unsellable, s.Unsellable)
	}
	if _, err := svc.Of(t.Context(), uuid.New(), "market"); err == nil {
		t.Error("an unknown valuation was accepted")
	}
}

// The kinds the targets share the worth out by (decision Р-37): the broker's
// papers by class, its roubles waiting to be put in, its dollars foreign
// currency and its gold a metal; dollars in cash foreign currency too; a
// dollar deposit a deposit; roubles on a card to live on and a broker known
// only by its balance outside the targets; a debt nowhere.
func TestTheTargetsShareOutTheWorthByKind(t *testing.T) {
	broker, card, dollars, deposit, other, credit := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	balance := func(id uuid.UUID, typ account.Type, currency string, minor int64) account.WithBalance {
		return account.WithBalance{
			Account: account.Account{ID: id, Type: typ, Currency: currency, Status: account.StatusActive},
			Balance: &account.BalancePoint{AmountMinor: minor},
		}
	}
	accounts := fakeAccounts{
		{Account: account.Account{ID: broker, Type: account.TypeBrokerage, Currency: "RUB", Status: account.StatusActive}},
		balance(card, account.TypeChecking, "RUB", 100_000_00),
		balance(dollars, account.TypeCash, "USD", 1_000_00),
		balance(deposit, account.TypeDeposit, "USD", 2_000_00),
		balance(other, account.TypeBrokerage, "RUB", 50_000_00),
		balance(credit, account.TypeCreditCard, "RUB", -30_000_00),
	}
	positions := fakePositions{broker: {
		Positions: []apitypes.Position{
			position(apitypes.InstrumentTypeShare, "RU0009029540", "RUB", of(300_000_00), of(300_000_00)),
			position(apitypes.InstrumentTypeEtf, "RU000A101X68", "RUB", of(100_000_00), of(100_000_00)),
		},
		Cash: []apitypes.CashPosition{
			{Currency: "RUB", AmountMinor: 20_000_00}, {Currency: "USD", AmountMinor: 500_00}, {Currency: "XAU", AmountMinor: 1_000},
		},
	}}
	s, err := structure.NewService(accounts, positions, fakeSpaces{}, fakeRates{}).Of(t.Context(), uuid.New(), structure.Liquid)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int64{}
	for _, sl := range s.ByKind {
		kinds[sl.Key] = sl.Minor
	}
	// $500 at the broker and $1 000 in cash: 135 000; 10 g of gold: 100 000.
	want := map[string]int64{
		"shares": 300_000_00, "funds": 100_000_00, "broker_cash": 20_000_00, "foreign_currency": 135_000_00,
		"metals": 100_000_00, "deposits": 180_000_00,
	}
	if len(kinds) != len(want) {
		t.Errorf("kinds = %v, want %v", kinds, want)
	}
	for k, v := range want {
		if kinds[k] != v {
			t.Errorf("kind %s = %d, want %d", k, kinds[k], v)
		}
	}
	if s.Targets != 835_000_00 || s.Everyday != 100_000_00 || s.Unsplit != 50_000_00 || s.Debts != -30_000_00 {
		t.Errorf("targets %d, everyday %d, unsplit %d, debts %d", s.Targets, s.Everyday, s.Unsplit, s.Debts)
	}
	currencies := map[string]int64{}
	for _, sl := range s.ByTargetCurrency {
		currencies[sl.Key] = sl.Minor
	}
	if currencies["RUB"] != 420_000_00 || currencies["USD"] != 315_000_00 || currencies["XAU"] != 100_000_00 {
		t.Errorf("target currencies = %v", currencies)
	}
}

// fakeWorths is a box whose bars would sell for 950 000 and are worth
// 1 000 000 by the central bank, one coin not valued yet.
type fakeWorths struct{ box uuid.UUID }

func (f fakeWorths) Worths(_ context.Context, _, id uuid.UUID, days []time.Time) ([]account.JournalValue, bool, error) {
	if id != f.box {
		return nil, false, nil
	}
	return []account.JournalValue{{
		Currency: "RUB", Operations: 3, Unpriced: 1, FullUnpriced: 1,
		ByCurrency: map[string]int64{"RUB": 950_000_00}, FullByCurrency: map[string]int64{"RUB": 1_000_000_00},
	}}, true, nil
}

// A valuables account's bars and coins are metals (decision Р-39): as they
// would sell at the liquid valuation, by the central bank's price at the
// full one; a piece not valued yet is counted unpriced.
func TestBarsAreMetals(t *testing.T) {
	box := uuid.New()
	accounts := fakeAccounts{{Account: account.Account{ID: box, Type: account.TypeValuables, Currency: "RUB", Status: account.StatusActive}}}
	svc := structure.NewService(accounts, fakePositions{}, fakeSpaces{}, fakeRates{}).WithValuables(fakeWorths{box})
	for valuation, want := range map[structure.Valuation]int64{structure.Liquid: 950_000_00, structure.Full: 1_000_000_00} {
		s, err := svc.Of(t.Context(), uuid.New(), valuation)
		if err != nil {
			t.Fatal(err)
		}
		if len(s.ByKind) != 1 || s.ByKind[0].Key != "metals" || s.ByKind[0].Minor != want || s.Targets != want || s.Unpriced != 1 {
			t.Errorf("%s: kinds %+v, targets %d, unpriced %d", valuation, s.ByKind, s.Targets, s.Unpriced)
		}
		if len(s.ByClass) != 1 || s.ByClass[0].Key != "metals" {
			t.Errorf("%s: classes %+v", valuation, s.ByClass)
		}
	}
}

type fakeClasses map[uuid.UUID]instrument.Class

func (f fakeClasses) ClassesFor(context.Context, uuid.UUID, []uuid.UUID) (map[uuid.UUID]instrument.Class, error) {
	return f, nil
}

// Papers by their country of risk, sector and where they are held (decision
// Р-46): Сбербанк — Russia, financials; FinEx's American fund — the United
// States, though Irish by ISIN; Apple, which the broker does not know — the
// ISIN's United States, marked so, of no sector; a fund at a broker abroad.
// The shares are of the papers and of what the brokers hold, not the worth.
func TestPapersComeApartByCountrySectorAndHolding(t *testing.T) {
	broker, abroad, card := uuid.New(), uuid.New(), uuid.New()
	sber, fxus, aapl, kz := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	paper := func(id uuid.UUID, typ apitypes.InstrumentType, isin, currency string, minor int64) apitypes.Position {
		p := position(typ, isin, currency, of(minor), of(minor))
		p.Instrument.Id = id
		return p
	}
	accounts := fakeAccounts{
		{Account: account.Account{ID: broker, Type: account.TypeBrokerage, Currency: "RUB", Status: account.StatusActive}},
		{Account: account.Account{ID: abroad, Type: account.TypeBrokerage, Currency: "USD", Status: account.StatusActive, ForeignBroker: true}},
		{Account: account.Account{ID: card, Type: account.TypeChecking, Currency: "RUB", Status: account.StatusActive}, Balance: &account.BalancePoint{AmountMinor: 50_000_00}},
	}
	positions := fakePositions{
		broker: {
			Positions: []apitypes.Position{
				paper(sber, apitypes.InstrumentTypeShare, "RU0009029540", "RUB", 300_000_00),
				paper(fxus, apitypes.InstrumentTypeEtf, "IE00BD3QHZ91", "RUB", 100_000_00),
			},
			Cash: []apitypes.CashPosition{{Currency: "RUB", AmountMinor: 20_000_00}},
		},
		abroad: {Positions: []apitypes.Position{
			paper(aapl, apitypes.InstrumentTypeShare, "US0378331005", "USD", 1_000_00),
			paper(kz, apitypes.InstrumentTypeEtf, "KZ0000000001", "USD", 1_000_00),
		}},
	}
	classes := fakeClasses{
		sber: {Country: "RU", Sector: instrument.SectorFinancials},
		fxus: {Country: "US", Sector: instrument.SectorFundsEquity},
		kz:   {Country: "KZ", Sector: instrument.SectorFundsEquity},
	}
	s, err := structure.NewService(accounts, positions, fakeSpaces{}, fakeRates{}).WithClasses(classes).Of(t.Context(), uuid.New(), structure.Liquid)
	if err != nil {
		t.Fatal(err)
	}
	got := func(list []structure.Slice) map[string]structure.Slice {
		m := map[string]structure.Slice{}
		for _, x := range list {
			m[x.Key] = x
		}
		return m
	}
	countries, sectors, held := got(s.ByCountry), got(s.BySector), got(s.ByHolding)
	if countries["RU"].Minor != 300_000_00 || countries["US"].Minor != 100_000_00+90_000_00 || countries["US"].ByISIN != 90_000_00 ||
		countries["KZ"].Minor != 90_000_00 || countries[""].Minor != 50_000_00+20_000_00 {
		t.Errorf("by country = %+v", s.ByCountry)
	}
	if sectors["financials"].Minor != 300_000_00 || sectors["funds_equity"].Minor != 190_000_00 || sectors["-"].Minor != 90_000_00 || sectors[""].Minor != 70_000_00 {
		t.Errorf("by sector = %+v", s.BySector)
	}
	if held["domestic"].Minor != 320_000_00 || held["foreign_paper"].Minor != 100_000_00 || held["foreign_broker"].Minor != 180_000_00 || held[""].Minor != 50_000_00 {
		t.Errorf("by holding = %+v", s.ByHolding)
	}
	if s.Papers != 580_000_00 || s.AtBrokers != 600_000_00 {
		t.Errorf("papers %d, at brokers %d", s.Papers, s.AtBrokers)
	}
}

// A planned purchase laid on the worth (decision Р-48): the paper on top of
// its account's, the money off its cash — even below zero, which is not a
// debt; an account not of the family's is refused.
func TestAPlanIsLaidOnTheWorth(t *testing.T) {
	broker := uuid.New()
	accounts := fakeAccounts{{Account: account.Account{ID: broker, Type: account.TypeBrokerage, Currency: "RUB", Status: account.StatusActive}}}
	positions := fakePositions{broker: {
		Positions: []apitypes.Position{position(apitypes.InstrumentTypeShare, "RU0009029540", "RUB", of(300_000_00), of(300_000_00))},
		Cash:      []apitypes.CashPosition{{Currency: "RUB", AmountMinor: 20_000_00}},
	}}
	svc := structure.NewService(accounts, positions, fakeSpaces{}, fakeRates{})
	fund := uuid.New()
	s, err := svc.OfWith(t.Context(), uuid.New(), structure.Liquid, []structure.Change{
		{Account: broker, Paper: &fund, Type: instrument.TypeETF, ISIN: "US46138G6492", Currency: "RUB", Minor: 50_000_00},
		{Account: broker, Currency: "RUB", Minor: -50_000_00},
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int64{}
	for _, k := range s.ByKind {
		kinds[k.Key] = k.Minor
	}
	if s.Assets != 320_000_00 || s.Debts != 0 || kinds["funds"] != 50_000_00 || kinds["broker_cash"] != -30_000_00 || s.Papers != 350_000_00 {
		t.Errorf("after the purchase: assets %d, debts %d, kinds %v, papers %d", s.Assets, s.Debts, kinds, s.Papers)
	}
	if _, err := svc.OfWith(t.Context(), uuid.New(), structure.Liquid, []structure.Change{{Account: uuid.New(), Currency: "RUB", Minor: 1}}); err == nil {
		t.Error("a stranger's account was taken")
	}
}

// What each broker and bank holds (decision Р-47): the card at «Тинькофф» is
// Т-Банк's, the broker abroad Freedom's, a savings account at a bank the
// catalog does not know «-»; cash and a credit card's debt are no bank's
// assets.
func TestTheWorthComesApartByBrokerAndBank(t *testing.T) {
	card, saving, broker, cash, credit := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	bal := func(v int64) *account.BalancePoint { return &account.BalancePoint{AmountMinor: v} }
	accounts := fakeAccounts{
		{Account: account.Account{ID: card, Type: account.TypeChecking, Currency: "RUB", Status: account.StatusActive, Institution: "Тинькофф"}, Balance: bal(100_000_00)},
		{Account: account.Account{ID: saving, Type: account.TypeSavings, Currency: "RUB", Status: account.StatusActive, Institution: "Свой банк"}, Balance: bal(300_000_00)},
		{Account: account.Account{ID: broker, Type: account.TypeBrokerage, Currency: "RUB", Status: account.StatusActive, Institution: "Freedom Finance"}, Balance: bal(500_000_00)},
		{Account: account.Account{ID: cash, Type: account.TypeCash, Currency: "RUB", Status: account.StatusActive}, Balance: bal(20_000_00)},
		{Account: account.Account{ID: credit, Type: account.TypeCreditCard, Currency: "RUB", Status: account.StatusActive, Institution: "Альфа-Банк"}, Balance: bal(-40_000_00)},
	}
	s, err := structure.NewService(accounts, fakePositions{}, fakeSpaces{}, fakeRates{}).Of(t.Context(), uuid.New(), structure.Liquid)
	if err != nil {
		t.Fatal(err)
	}
	got := func(list []structure.Slice) map[string]int64 {
		m := map[string]int64{}
		for _, x := range list {
			m[x.Key] = x.Minor
		}
		return m
	}
	if banks := got(s.ByBank); len(banks) != 2 || banks["tbank"] != 100_000_00 || banks["-"] != 300_000_00 || s.AtBanks != 400_000_00 {
		t.Errorf("by bank = %v, at banks %d", s.ByBank, s.AtBanks)
	}
	if brokers := got(s.ByBroker); len(brokers) != 1 || brokers["freedom"] != 500_000_00 {
		t.Errorf("by broker = %v", s.ByBroker)
	}
}
