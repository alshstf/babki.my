package operation_test

import (
	"errors"
	"math"
	"testing"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/money"
)

// A quantity and a price are the two factors of one product, and that product
// is money: it is what the trade was for. Everything that later reads the
// journal multiplies the quantity by a price — a quote's, not this operation's
// — and then by an fx rate, and the result has to be an int64 of minor units.
// Plan 10 made a product that does not fit refuse to be published instead of
// wrapping, but that refusal arrives on the READ: the position screen answers
// 500 for as long as the row exists, and the only repair is to find and delete
// it. The same figure caught on the WRITE is a rejected field.
//
// These tests are about the write side. The read side keeps its own guard —
// see TestRowsWrittenBeforeTheBoundAreStillWorkable for why it has to.

// The bounds as strings, spelled out rather than derived from the package's own
// constants (they are unexported, and a test that computed them the same way the
// code does would agree with a wrong bound).
//
// The first two are the SAME NUMBER, and that is not a slip: a count of units
// and a sum of money per unit are both the money cap read in major units (see
// operation.maxQuantity). Because they print alike — and because 10^13 is a
// substring of the 10^15 the product cap prints, and 10^10 a substring of both —
// no assertion in this file looks for a bound INSIDE a message. Every refusal is
// compared whole, by wantRefusal. The first version of these tests checked for
// substrings, and a price refusal that printed the QUANTITY bound went unnoticed
// by every test in the package: the message named a number the importer would
// then rescale their price column to, and be refused again.
//
// About that exact swap, one honest note. Now that the two bounds are the same
// number, printing the quantity bound in the price message produces the very
// same string, so it is no longer a lie and no test can — or should — redden at
// it. What whole comparison does catch, and what substring checks did not, is
// every swap that changes the message: either refusal naming the PRODUCT cap,
// the product refusal naming a factor's bound, or any message naming the wrong
// field. Those were mutated one at a time and every one of them reddens.
const (
	quantityBound   = "10000000000000"   // 10^13 units
	priceBound      = "10000000000000"   // 10^13 major currency units per unit
	productBound    = "1000000000000000" // 10^15 minor units of money
	splitRatioBound = "10000000000"      // 10^10 new units per old one

	// moneyBound is the cap on the money fields THEMSELVES — an operation's
	// amount, its fee, and the cost basis a transfer may be handed. It prints
	// the same digits as productBound because in the code it is the same
	// constant: money.MaxAmountMinor bounds the product of the two factors and
	// each of these three fields alike, which is the whole argument for there
	// being one such constant (see its doc comment). It is written out here for
	// the reason the block above gives — a test that derived the bound the way
	// the code does would agree with a wrong bound as readily as with the right
	// one — and the refusals are still compared WHOLE, so a message that swapped
	// one of these fields for another still reddens.
	moneyBound = "1000000000000000" // 10^15 minor units of money
)

// moneyBoundInt is moneyBound as the number, for building the figures the
// requests below carry. Parsed from the very digits the messages are asserted
// against, so the two cannot be moved apart.
var moneyBoundInt = decimal.RequireFromString(moneyBound).IntPart()

// wantRefusal asserts the WHOLE refusal, not fragments of it. A bound is only
// useful if the message names the field it belongs to and the number that
// applies to that field: this is the one line that tells an importer which
// column it mis-scaled, and a message naming another field's bound sends them to
// rescale the wrong column. Since three of the four bounds print digits that are
// substrings of one another, whole comparison is the only assertion that can
// tell the messages apart at all.
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

// TestQuantityBeyondTheBoundIsRefusedAtTheWrite is the case from #84 as it
// actually arrives: an import whose quantity column was scaled wrong, carrying
// no price at all — many broker exports have none. 10^17 shares was accepted by
// every check there was — positive, on the journal's decimal scale, comfortably
// inside NUMERIC(30,10) — and the position it made cannot be valued at any
// ordinary price.
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

	// And one unit past the bound, which is where the bound actually is. The
	// mis-scaled import above would be refused by any cap loose enough to be no
	// cap at all; this fixes the edge, and together with the acceptance test
	// below it leaves exactly one quantity that is the largest allowed.
	buy.Quantity = dec("10000000000001") // 10^13 + 1
	_, err = svc.Create(f.ctx, f.spaceID, buy)
	wantRefusal(t, err, "quantity must be within ±"+quantityBound)
}

// TestQuantityExactlyAtTheBoundIsAccepted is the other side of the same guard,
// and it carries the promise the bound is CHOSEN for. A bound that refuses the
// value ON it withholds a figure that is perfectly representable and says
// nothing about why — the same class of harm as accepting one that wraps.
//
// The second half is what makes the number the right one rather than merely a
// number. The largest quantity accepted here has to survive the arithmetic the
// positions screen does to it, or the write-time bound is a refusal that changes
// nothing: at 10^15 units a quote of 92.24 — less than an ordinary share costs —
// already overflowed, so the screen went on answering 500 for rows this very
// check had accepted. Both halves live in one test on purpose: as two, one could
// be moved and the other left to disagree with it, which is exactly what
// happened when the write-side bound and portfolio's read-side test each stated
// their own verdict on 10^15 and neither mentioned the other.
func TestQuantityExactlyAtTheBoundIsAccepted(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// 10^13 units at one rouble each: the notional is exactly the cap on
	// amount_minor, so nothing else in validate has anything to object to.
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

	// And the screen can still value it. This is money.Minor(price × quantity ×
	// 100) — the arithmetic of portfolio.marketValue for a share, refused by the
	// same function it refuses with — at the largest whole-currency quote that
	// fits, 9223 per unit. Its twin over there
	// (TestMarketValuePublishesTheLargestQuantityAWriteAccepts) asserts the same
	// pair of numbers through marketValue itself.
	const largestOrdinaryQuote = 9223
	q := created.Quantity.Mul(decimal.NewFromInt(largestOrdinaryQuote)).Shift(2)
	if _, err := money.Minor(q); err != nil {
		t.Errorf("the largest accepted quantity at a quote of %d: %v — a bound that admits a quantity the screen cannot render is not a bound",
			largestOrdinaryQuote, err)
	}
	// One unit of money more per unit of instrument does not fit, which is what
	// makes 9223 the edge rather than a comfortable round number.
	if _, err := money.Minor(created.Quantity.Mul(decimal.NewFromInt(largestOrdinaryQuote + 1)).Shift(2)); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("quote of %d at the bound: err = %v, want ErrOverflow — the margin is narrower than this test claims",
			largestOrdinaryQuote+1, err)
	}
}

// TestPriceBeyondTheBoundIsRefusedAtTheWrite covers the other factor. The
// quantity here is tiny, so their product is ordinary money and only the price
// itself is out of range — a price above the bound is more money for ONE unit
// than a whole operation is allowed to be for.
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

// TestPriceExactlyAtTheBoundIsAccepted mirrors the quantity case at the other
// factor's edge.
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

// TestPriceTimesQuantityBeyondTheMoneyCapIsRefused is the check the two
// per-field bounds cannot make between them. Each factor here is far inside its
// own limit — 10^8 units is a hundred-thousandth of the quantity bound, 10^6
// apiece a ten-millionth of the price bound — and the product is 10^16 minor
// units, ten times the most money one operation is allowed to be for.
//
// The margin is deliberately that narrow. The product is 10^14 in MAJOR units,
// which is inside the cap when the cap is read as major units, so a check that
// forgot to shift the product into minor ones — the units the cap is in — would
// wave this through and no other test here would notice.
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

// TestSplitRatioBeyondTheBoundIsRefused closes the same hole one column over.
// split_ratio was checked for positivity and for nothing else, and it is the
// field with the longest reach of the three: applySplit multiplies the WHOLE
// position's quantity by it, a path with no bound anywhere along it, so a ratio
// no user would type takes an ordinary holding past anything the screen can
// value.
//
// The bound is where the column ends — NUMERIC(20,10) keeps ten integer digits —
// so this is also the difference between a named field in a 400 and a Postgres
// error about numeric overflow arriving as a 500.
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

// TestSplitRatioJustInsideTheBoundIsAccepted is the other side of it. The bound
// is the first value the column cannot hold, so — alone among the bounds here —
// the value ON it is refused and the largest one accepted is the largest the
// column can actually keep. A guard that stopped earlier would refuse a ratio
// the journal has room for and say nothing about why.
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

// TestTransferQuantityIsBoundedToo closes the other door into the same column.
// A transfer writes the moved quantity into two rows of its own, and it does
// not go through validate at all.
//
// It is not merely symmetry: the bound is per OPERATION, and a position is the
// sum of many, so a holding can legitimately grow past the bound one accepted
// buy at a time and then be moved in one transfer.
func TestTransferQuantityIsBoundedToo(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// Two accepted buys of 10^13 each leave a position of 2×10^13 — above the
	// per-operation bound, by rows every check passed.
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

// TestRowsWrittenBeforeTheBoundAreStillWorkable is the promise the change owes
// the data that is already there. No migration comes with the bound: a journal
// written before it keeps whatever it holds, and the only thing that changes is
// what may be ADDED.
//
// Both halves matter. An account carrying such a row must still accept ordinary
// operations — validate looks at the candidate, never at the journal it joins —
// and the offending row must still be deletable, since deleting it is the one
// repair available. A bound applied on the way OUT of the database would take
// both away: nothing could be written to the account and nothing removed from
// it, which is the state this whole plan exists to avoid.
func TestRowsWrittenBeforeTheBoundAreStillWorkable(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// Straight through the store, which is how such a row got there: the bound
	// lives in the service, and the service is what did not have it.
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

// The three money fields below were bounded by the server and pinned by
// nothing. Every test above is about a FACTOR — a quantity, a price, a ratio —
// and the money those factors stand for was left to the product check, which
// only fires when both of them are present. amount_minor, fee_minor and a
// transfer's cost_minor are money in their own right, arrive on their own, and
// each has its own refusal; until now each of those refusals could have been
// moved, loosened or dropped with the whole package still green.
//
// They are pinned HERE rather than left implied because the contract now states
// them (#100, #102): api/openapi.yaml declares minimum/maximum on all three so a
// client can validate before it sends, and
// money.TestTheContractStatesTheBoundTheServerEnforces ties those numbers to
// money.MaxAmountMinor. That test can only say the document and the constant
// agree. What makes the constant the SERVER's answer rather than a number in a
// file is below: the value on each bound is accepted and the one past it is
// refused, by name and by the whole sentence.

// TestAmountExactlyAtTheBoundIsAcceptedAndPastItRefused fixes both edges of
// amount_minor. The bound is on the MAGNITUDE — an outflow is an ordinary
// negative amount — so both ends are real doors, and both are compared
// explicitly rather than through an abs(): negating math.MinInt64 is itself an
// overflow, so a check written as abs() would let the one figure through that
// no later arithmetic survives.
func TestAmountExactlyAtTheBoundIsAcceptedAndPastItRefused(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// The cap itself, in both directions. A bound that refused the value ON it
	// would withhold a figure that is perfectly representable and say nothing
	// about why.
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

	// And one minor unit past it, which is where the bound actually is, plus the
	// two figures that break the arithmetic outright. math.MinInt64 is the reason
	// the check reads as two comparisons.
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

// TestFeeIsBoundedAtZeroAndAtTheCap fixes fee_minor's two edges, and they are
// NOT the two edges of amount_minor: a fee is money that was charged, never
// money that came back, so the floor is zero and the refusal for a negative one
// is a sentence of its own. That asymmetry is the reason the contract's
// declaration for this field could not be copied from the amount beside it — a
// floor of -10^15 there would have promised a client that a negative fee is
// sendable, and every such request would have come back 400.
func TestFeeIsBoundedAtZeroAndAtTheCap(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// Both ends of what is allowed: no fee at all, and a fee at the cap. On two
	// dates because a deposit is an ordinary row and two of them may as well be
	// two days apart.
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

// TestTransferCostOverrideIsBoundedAtZeroAndAtTheCap covers the third field,
// and the one door that reaches it: a cost basis handed to a transfer instead
// of computed from the source account's history. It does not go through
// validate at all — CreateTransfer bounds it itself — so nothing above says
// anything about it.
//
// Its floor is zero for a reason of its own: the figure is what the shares
// COST, and a negative cost is not a direction, it is a number that would make
// every later profit larger than the sale it came from.
func TestTransferCostOverrideIsBoundedAtZeroAndAtTheCap(t *testing.T) {
	f := newFixture(t)
	svc := operation.NewService(f.store)

	// Enough shares to move several times over, so each acceptance below has
	// something left to move.
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

	// Zero is a real basis and not an empty field: shares can arrive at no cost
	// at all, and the queue behind them is what a hand-given basis replaces.
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
