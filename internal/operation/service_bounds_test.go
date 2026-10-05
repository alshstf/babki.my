package operation_test

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/money"
	"babki.my/babki/internal/platform/testdb"
	"babki.my/babki/internal/portfolio"
)

// Write-side bounds on quantity, price and their product. A product that does
// not fit is refused on read too, but there it is a position screen answering 500
// until the row is found and deleted; on write it is a rejected field.
// TestRowsWrittenBeforeTheBoundAreStillWorkable covers why the read side keeps its
// guard.

// The bounds spelled out, not derived from the package's constants, so a wrong
// bound cannot be agreed with. Several print digits that are substrings of others
// (10^10, 10^13, 10^15), so every refusal is compared whole by wantRefusal: a
// message naming another field's bound would send an importer to rescale the
// wrong column.
const (
	quantityBound   = "10000000000000"   // 10^13 units
	priceBound      = "10000000000000"   // 10^13 major currency units per unit
	productBound    = "1000000000000000" // 10^15 minor units of money
	splitRatioBound = "10000000000"      // 10^10 new units per old one

	// moneyBound caps amount, fee and a transfer's given basis: the same constant
	// as the product cap, money.MaxAmountMinor.
	moneyBound = "1000000000000000" // 10^15 minor units of money
)

// moneyBoundInt is parsed from the same digits the messages are checked
// against.
var moneyBoundInt = decimal.RequireFromString(moneyBound).IntPart()

// wantRefusal asserts the whole refusal: the field and the number that applies
// to it.
func wantRefusal(t *testing.T, err error, want string) {
	t.Helper()
	if !errors.Is(err, family.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	full := family.ErrValidation.Error() + ": " + want
	if got := err.Error(); got != full {
		t.Errorf("refusal = %q, want exactly %q", got, full)
	}
}

// #84 as it arrives: a mis-scaled quantity column with no price. 10^17 shares
// passed every older check and made a position no ordinary quote can value.
func TestQuantityBeyondTheBoundIsRefusedAtTheWrite(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("1e17"),
		AmountMinor: -10_000_000, Currency: "RUB",
	}
	_, err := svc.Create(f.ctx, f.spaceID, buy)
	wantRefusal(t, err, "quantity must be within ±"+quantityBound)

	// One unit past the bound, which is where the bound actually is.
	buy.Quantity = dec("10000000000001") // 10^13 + 1
	_, err = svc.Create(f.ctx, f.spaceID, buy)
	wantRefusal(t, err, "quantity must be within ±"+quantityBound)
}

// The bound itself is accepted, and the largest accepted quantity must
// survive the positions screen's arithmetic at an ordinary quote, or the bound
// changes nothing (at 10^15 units, a quote of 92.24 already overflowed). Both
// halves in one test so they cannot be moved apart.
func TestQuantityExactlyAtTheBoundIsAccepted(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// 10^13 units at one rouble: exactly the amount cap.
	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec(quantityBound), Price: dec("1"),
		AmountMinor: -1_000_000_000_000_000, Currency: "RUB",
	}
	created, err := svc.Create(f.ctx, f.spaceID, buy)
	if err != nil {
		t.Fatalf("buy of exactly %s units: %v — the value ON the bound is inside it", quantityBound, err)
	}
	if created.Quantity == nil || created.Quantity.String() != quantityBound {
		t.Errorf("stored quantity = %v, want %s", created.Quantity, quantityBound)
	}

	// portfolio.marketValue's arithmetic for a share, money.Minor(price ×
	// quantity × 100), at the largest whole quote that fits: 9223. Its twin is
	// TestMarketValuePublishesTheLargestQuantityAWriteAccepts.
	const largestOrdinaryQuote = 9223
	q := created.Quantity.Mul(decimal.NewFromInt(largestOrdinaryQuote)).Shift(2)
	if _, err := money.Minor(q); err != nil {
		t.Errorf("the largest accepted quantity at a quote of %d: %v — a bound that admits a quantity the screen cannot render is not a bound",
			largestOrdinaryQuote, err)
	}
	// 9224 does not fit.
	if _, err := money.Minor(created.Quantity.Mul(decimal.NewFromInt(largestOrdinaryQuote + 1)).Shift(2)); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("quote of %d at the bound: err = %v, want ErrOverflow — the margin is narrower than this test claims",
			largestOrdinaryQuote+1, err)
	}
}

// A tiny quantity at a price past the bound: only the price is refused.
func TestPriceBeyondTheBoundIsRefusedAtTheWrite(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("0.0001"), Price: dec("10000000000001"),
		AmountMinor: -1_000_000, Currency: "RUB",
	}
	_, err := svc.Create(f.ctx, f.spaceID, buy)
	wantRefusal(t, err, "price must be within ±"+priceBound+" per unit")
}

// The price bound itself is accepted.
func TestPriceExactlyAtTheBoundIsAccepted(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// One ten-thousandth of a unit at 10^13 apiece: 10^9 in money, ordinary.
	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("0.0001"), Price: dec(priceBound),
		AmountMinor: -100_000_000_000, Currency: "RUB",
	}
	if _, err := svc.Create(f.ctx, f.spaceID, buy); err != nil {
		t.Fatalf("buy at a price of exactly %s: %v — the value ON the bound is inside it", priceBound, err)
	}
}

// Each factor is well inside its bound but the product is 10^16 minor units,
// ten times the cap. In major units it is 10^14, inside the cap, so a check that
// forgot the shift to minor units would pass this.
func TestPriceTimesQuantityBeyondTheMoneyCapIsRefused(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("1e8"), Price: dec("1e6"),
		AmountMinor: -1_000_000, Currency: "RUB",
	}
	_, err := svc.Create(f.ctx, f.spaceID, buy)
	wantRefusal(t, err, "price × quantity must be within ±"+productBound+" minor units")
}

// applySplit multiplies the whole position by split_ratio with no bound on
// the way. The bound is the column's end (NUMERIC(20,10)), so this is also a named
// 400 instead of a numeric-overflow 500.
func TestSplitRatioBeyondTheBoundIsRefused(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("1e6"), Price: dec("100"),
		AmountMinor: -10_000_000_000, Currency: "RUB",
	}
	if _, err := svc.Create(f.ctx, f.spaceID, buy); err != nil {
		t.Fatalf("seed the position: %v", err)
	}

	split := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSplit,
		OccurredOn: date("2026-07-02"), SplitRatio: dec(splitRatioBound),
		AmountMinor: 0, Currency: "RUB",
	}
	_, err := svc.Create(f.ctx, f.spaceID, split)
	wantRefusal(t, err, "split_ratio must be less than "+splitRatioBound)
}

// The largest ratio the column keeps is accepted; the bound itself is not.
func TestSplitRatioJustInsideTheBoundIsAccepted(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	buy := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}
	if _, err := svc.Create(f.ctx, f.spaceID, buy); err != nil {
		t.Fatalf("seed the position: %v", err)
	}

	// Every digit NUMERIC(20,10) has: ten before the point and ten after.
	const largest = "9999999999.9999999999"
	split := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeSplit,
		OccurredOn: date("2026-07-02"), SplitRatio: dec(largest),
		AmountMinor: 0, Currency: "RUB",
	}
	created := seedSplit(t, f, svc, split)
	if created.SplitRatio == nil || created.SplitRatio.String() != largest {
		t.Errorf("stored split_ratio = %v, want %s", created.SplitRatio, largest)
	}
}

// A transfer writes the moved quantity without going through validate, and a
// position can grow past the per-operation bound one buy at a time.
func TestTransferQuantityIsBoundedToo(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// Two accepted buys of 10^13 make a position of 2×10^13.
	for _, on := range []string{"2026-07-01", "2026-07-02"} {
		buy := operation.Operation{
			AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
			OccurredOn: date(on), Quantity: dec(quantityBound), Price: dec("1"),
			AmountMinor: -1_000_000_000_000_000, Currency: "RUB",
		}
		if _, err := svc.Create(f.ctx, f.spaceID, buy); err != nil {
			t.Fatalf("buy on %s: %v", on, err)
		}
	}

	_, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
		FromAccountID: f.accountID, ToAccountID: f.account2ID, InstrumentID: f.sberID,
		Quantity: *dec("20000000000000"), OccurredOn: date("2026-07-03"),
	})
	wantRefusal(t, err, "quantity must be within ±"+quantityBound)
}

// Rows written before the bound (no migration came with it) leave the account
// usable: ordinary operations are still accepted, since validate looks only at
// the candidate, and the oversized row can still be deleted.
func TestRowsWrittenBeforeTheBoundAreStillWorkable(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// Straight through the store, as such a row got there.
	monster := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("1e17"), Price: dec("0.000001"),
		AmountMinor: -10_000_000, Currency: "RUB",
	}
	stored, err := f.store.Create(f.ctx, f.spaceID, monster, func(operation.Operation) error { return nil })
	if err != nil {
		t.Fatalf("seed the pre-existing row: %v", err)
	}

	ordinary := operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-02"), Quantity: dec("10"), Price: dec("100"),
		AmountMinor: -100_000, Currency: "RUB",
	}
	if _, err := svc.Create(f.ctx, f.spaceID, ordinary); err != nil {
		t.Fatalf("ordinary buy on an account holding an over-bound row: %v — the bound checks the candidate, not the journal", err)
	}

	if err := svc.Delete(f.ctx, f.spaceID, stored.ID); err != nil {
		t.Fatalf("delete the over-bound row: %v — deleting it is the only repair there is", err)
	}
}

// The money fields themselves: amount_minor, fee_minor and a transfer's
// cost_minor. openapi declares these bounds (#100, #102) and
// money.TestTheContractStatesTheBoundTheServerEnforces ties them to the constant;
// these tests show the server applies them.

// amount_minor is bounded in magnitude, both ends compared explicitly:
// abs(math.MinInt64) overflows.
func TestAmountExactlyAtTheBoundIsAcceptedAndPastItRefused(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// The cap itself, both directions.
	for _, tc := range []struct {
		what   string
		typ    operation.Type
		amount int64
	}{
		{"deposit of exactly the cap", operation.TypeDeposit, moneyBoundInt},
		{"withdrawal of exactly the cap", operation.TypeWithdrawal, -moneyBoundInt},
	} {
		op := operation.Operation{
			AccountID: f.accountID, Type: tc.typ, OccurredOn: date("2026-07-01"),
			AmountMinor: tc.amount, Currency: "RUB",
		}
		created, err := svc.Create(f.ctx, f.spaceID, op)
		if err != nil {
			t.Fatalf("%s: %v — the value ON the bound is inside it", tc.what, err)
		}
		if created.AmountMinor != tc.amount {
			t.Errorf("stored amount after a %s = %d, want %d", tc.what, created.AmountMinor, tc.amount)
		}
	}

	// One minor unit past it, and the two figures that break arithmetic.
	for _, tc := range []struct {
		what   string
		typ    operation.Type
		amount int64
	}{
		{"deposit one minor unit past the cap", operation.TypeDeposit, moneyBoundInt + 1},
		{"withdrawal one minor unit past the cap", operation.TypeWithdrawal, -moneyBoundInt - 1},
		{"deposit of math.MaxInt64", operation.TypeDeposit, math.MaxInt64},
		{"withdrawal of math.MinInt64", operation.TypeWithdrawal, math.MinInt64},
	} {
		op := operation.Operation{
			AccountID: f.accountID, Type: tc.typ, OccurredOn: date("2026-07-02"),
			AmountMinor: tc.amount, Currency: "RUB",
		}
		_, err := svc.Create(f.ctx, f.spaceID, op)
		t.Run(tc.what, func(t *testing.T) {
			wantRefusal(t, err, "amount_minor must be within ±"+moneyBound)
		})
	}
}

// A fee is never money back, so its floor is zero with its own refusal, unlike
// amount_minor's.
func TestFeeIsBoundedAtZeroAndAtTheCap(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// No fee, and a fee at the cap.
	for _, tc := range []struct {
		on  string
		fee int64
	}{
		{"2026-07-01", 0},
		{"2026-07-02", moneyBoundInt},
	} {
		fee := tc.fee
		op := operation.Operation{
			AccountID: f.accountID, Type: operation.TypeDeposit, OccurredOn: date(tc.on),
			AmountMinor: 100_000, FeeMinor: fee, Currency: "RUB",
		}
		created, err := svc.Create(f.ctx, f.spaceID, op)
		if err != nil {
			t.Fatalf("deposit with a fee of %d: %v — the value ON the bound is inside it", fee, err)
		}
		if created.FeeMinor != fee {
			t.Errorf("stored fee = %d, want %d", created.FeeMinor, fee)
		}
	}

	for _, tc := range []struct {
		what string
		fee  int64
		want string
	}{
		{"a fee of one minor unit below zero", -1, "fee_minor must be >= 0"},
		{"a fee of math.MinInt64", math.MinInt64, "fee_minor must be >= 0"},
		{"a fee one minor unit past the cap", moneyBoundInt + 1, "fee_minor must be <= " + moneyBound},
		{"a fee of math.MaxInt64", math.MaxInt64, "fee_minor must be <= " + moneyBound},
	} {
		op := operation.Operation{
			AccountID: f.accountID, Type: operation.TypeDeposit, OccurredOn: date("2026-07-03"),
			AmountMinor: 100_000, FeeMinor: tc.fee, Currency: "RUB",
		}
		_, err := svc.Create(f.ctx, f.spaceID, op)
		t.Run(tc.what, func(t *testing.T) { wantRefusal(t, err, tc.want) })
	}
}

// A basis handed to a transfer is bounded by CreateTransfer itself, at zero
// and at the cap: a negative cost would inflate every later profit.
func TestTransferCostOverrideIsBoundedAtZeroAndAtTheCap(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// Enough shares for several moves.
	if _, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
		AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
		OccurredOn: date("2026-07-01"), Quantity: dec("100"), Price: dec("100"),
		AmountMinor: -1_000_000, Currency: "RUB",
	}); err != nil {
		t.Fatalf("seed the source position: %v", err)
	}

	move := func(on string, cost int64) error {
		_, _, err := svc.CreateTransfer(f.ctx, f.spaceID, operation.TransferParams{
			FromAccountID: f.accountID, ToAccountID: f.account2ID, InstrumentID: f.sberID,
			Quantity: *dec("10"), OccurredOn: date(on), CostMinorOverride: &cost,
		})
		return err
	}

	// Zero is a real basis: shares can arrive at no cost.
	if err := move("2026-07-02", 0); err != nil {
		t.Fatalf("transfer with a hand-given basis of 0: %v — zero is inside the bound", err)
	}
	if err := move("2026-07-03", moneyBoundInt); err != nil {
		t.Fatalf("transfer with a hand-given basis of exactly the cap: %v — the value ON the bound is inside it", err)
	}

	for _, tc := range []struct {
		what string
		cost int64
	}{
		{"a hand-given basis of one minor unit below zero", -1},
		{"a hand-given basis of math.MinInt64", math.MinInt64},
		{"a hand-given basis one minor unit past the cap", moneyBoundInt + 1},
		{"a hand-given basis of math.MaxInt64", math.MaxInt64},
	} {
		err := move("2026-07-04", tc.cost)
		t.Run(tc.what, func(t *testing.T) {
			wantRefusal(t, err, "cost_minor must be within 0.."+moneyBound)
		})
	}
}

// settled_on is not before its trade nor more than a year after (#202).
func TestSettledOnIsHeldToTheTradeItSettles(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)
	buy := func(settled string) error {
		on := date(settled)
		_, err := svc.Create(f.ctx, f.spaceID, operation.Operation{
			AccountID: f.accountID, InstrumentID: &f.sberID, Type: operation.TypeBuy,
			OccurredOn: date("2026-03-02"), SettledOn: &on, Quantity: dec("1"), Price: dec("100"),
			AmountMinor: -10_000, Currency: "RUB",
		})
		return err
	}
	for _, ok := range []string{"2026-03-02", "2026-03-04", "2027-03-02"} {
		if err := buy(ok); err != nil {
			t.Errorf("settled_on %s: %v, want accepted", ok, err)
		}
	}
	for _, bad := range []string{"2026-03-01", "2027-03-04", "9999-12-31"} {
		if err := buy(bad); !errors.Is(err, family.ErrValidation) {
			t.Errorf("settled_on %s: err = %v, want ErrValidation", bad, err)
		}
	}
}

// The table's CHECK and portfolio.Types list the same types; a mismatch is a
// 500 on the first write of the missing one.
func TestTheSchemaNamesExactlyTheOperationTypesTheCodeKnows(t *testing.T) {
	f := newFixture(t)
	inCode := make([]string, 0, len(portfolio.Types()))
	for _, typ := range portfolio.Types() {
		inCode = append(inCode, string(typ))
	}
	if inSchema := testdb.CheckLiterals(t, f.pool, "operations_type_check"); !slices.Equal(inSchema, inCode) {
		t.Errorf("the schema allows %v, the code knows %v", inSchema, inCode)
	}
}

// TradeAmountMinor against the table the trade dialog's preview also uses
// (web/src/lib/money.test.ts).
func TestTradeAmountMinorFollowsTheSharedTable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "lib", "testdata", "trade-amounts.json"))
	if err != nil {
		t.Fatalf("read the table: %v", err)
	}
	var table struct {
		Cases []struct {
			Quantity string `json:"quantity"`
			Price    string `json:"price"`
			Minor    int64  `json:"minor"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatalf("decode the table: %v", err)
	}
	if len(table.Cases) == 0 {
		t.Fatal("the table is empty")
	}
	for _, c := range table.Cases {
		qty, price := decimal.RequireFromString(c.Quantity), decimal.RequireFromString(c.Price)
		if got, err := operation.TradeAmountMinor(operation.TypeSell, qty, price); err != nil || got != c.Minor {
			t.Errorf("sell %s × %s = %d (%v), want %d", c.Quantity, c.Price, got, err, c.Minor)
		}
		if got, err := operation.TradeAmountMinor(operation.TypeBuy, qty, price); err != nil || got != -c.Minor {
			t.Errorf("buy %s × %s = %d (%v), want %d", c.Quantity, c.Price, got, err, -c.Minor)
		}
	}
}
