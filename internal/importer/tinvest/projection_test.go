package tinvest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/portfolio"
)

// This file is package tinvest (not tinvest_test) so it can reach mskDay,
// journalQuantity and the wire decoder the fixtures go through — all
// unexported, and all of them rules this task is about.
//
// EVERY EXPECTED VALUE BELOW IS A LITERAL. Not one of them is computed from a
// constant of the implementation: a projection whose expectations move with
// the code it checks would stay green through a change of rule, which is the
// exact way a whole family of window tests in this repository once went blind.

// Fixed ids, so an external id can be compared against a literal string.
var (
	fixtureRowID     = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	fixtureAccountID = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	fixtureInstrID   = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	otherInstrID     = uuid.MustParse("44444444-4444-4444-8444-444444444444")
)

func resolvedShare() *Resolved {
	return &Resolved{InstrumentID: fixtureInstrID, Type: instrument.TypeShare}
}

// loadOperationItem reads one fixture the way the client reads the wire: the
// raw document through wireOperationItem, so the fixtures stay in the shape
// the gateway actually sends (int64 fields as JSON STRINGS) rather than in
// whatever shape happens to be convenient for a test.
func loadOperationItem(t *testing.T, name string) OperationItem {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "ops", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var w wireOperationItem
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	it, err := w.parse(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return it
}

// mirrorRowFor builds the mirror row the store would write for one broker
// operation. It goes through the same helpers the insert does
// (upperCurrency, moneyOrNothing, contentKey — see mirrorInsertArgs), so a
// row cannot reach the projection here in a shape the database would never
// hold.
func mirrorRowFor(t *testing.T, name string) MirrorRow {
	t.Helper()
	it := loadOperationItem(t, name)
	return MirrorRow{
		ID:                 fixtureRowID,
		ConnectionID:       uuid.MustParse("55555555-5555-4555-8555-555555555555"),
		LinkID:             uuid.MustParse("66666666-6666-4666-8666-666666666666"),
		BrokerOperationID:  it.ID,
		ParentOperationID:  it.ParentOperationID,
		OpType:             it.Type,
		State:              it.State,
		OccurredAt:         it.Date.UTC(),
		Currency:           upperCurrency(it.Payment.Currency),
		Payment:            it.Payment.Decimal(),
		Price:              moneyOrNothing(it.Price),
		Commission:         moneyOrNothing(it.Commission),
		CommissionCurrency: upperCurrency(it.Commission.Currency),
		AccruedInt:         moneyOrNothing(it.AccruedInt),
		Quantity:           it.Quantity,
		QuantityDone:       it.QuantityDone,
		FIGI:               it.FIGI,
		InstrumentUID:      it.InstrumentUID,
		PositionUID:        it.PositionUID,
		AssetUID:           it.AssetUID,
		InstrumentType:     it.InstrumentType,
		Description:        it.Description,
		Raw:                it.Raw,
		ContentKey:         contentKey(it),
		FirstSeenAt:        time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC),
		LastConfirmedAt:    time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC),
	}
}

func projectOne(t *testing.T, row MirrorRow, resolved *Resolved) operation.Operation {
	t.Helper()
	ops, _, refusal := ProjectRow(row, fixtureAccountID, resolved, nil)
	if refusal != nil {
		t.Fatalf("ProjectRow(%s): refused: %v", row.OpType, refusal)
	}
	if len(ops) != 1 {
		t.Fatalf("ProjectRow(%s): got %d operations, want 1", row.OpType, len(ops))
	}
	return ops[0]
}

func day(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse day %q: %v", s, err)
	}
	return d
}

func TestProjectRowBuyIsRecordedOnTheMoscowDay(t *testing.T) {
	row := mirrorRowFor(t, "buy.json")
	op := projectOne(t, row, resolvedShare())

	// The broker's instant is 2026-03-14T21:30:00Z — still the 14th in UTC,
	// already the 15th in Moscow. The journal day is Moscow's.
	if want := day(t, "2026-03-15"); !op.OccurredOn.Equal(want) {
		t.Errorf("occurred_on = %s, want %s", op.OccurredOn.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if op.Type != operation.TypeBuy {
		t.Errorf("type = %s, want buy", op.Type)
	}
	if op.AmountMinor != -2750000 {
		t.Errorf("amount_minor = %d, want -2750000", op.AmountMinor)
	}
	if op.FeeMinor != 825 {
		t.Errorf("fee_minor = %d, want 825", op.FeeMinor)
	}
	if op.Quantity == nil || op.Quantity.String() != "100" {
		t.Errorf("quantity = %v, want 100", op.Quantity)
	}
	if op.Price == nil || op.Price.String() != "275" {
		t.Errorf("price = %v, want 275", op.Price)
	}
	if op.Currency != "RUB" {
		t.Errorf("currency = %q, want RUB", op.Currency)
	}
	if op.InstrumentID == nil || *op.InstrumentID != fixtureInstrID {
		t.Errorf("instrument_id = %v, want %s", op.InstrumentID, fixtureInstrID)
	}
	if op.AccountID != fixtureAccountID {
		t.Errorf("account_id = %s, want %s", op.AccountID, fixtureAccountID)
	}
	if op.Source != "tinvest" {
		t.Errorf("source = %q, want tinvest", op.Source)
	}
	if op.Note != "Покупка 100 шт." {
		t.Errorf("note = %q, want the broker's own description", op.Note)
	}
	// The suffix is on a single-entry row too: see withExternalIDs for why a
	// bare id would rename itself the day this trade grew a second entry.
	if op.ExternalID == nil || *op.ExternalID != "11111111-1111-4111-8111-111111111111/1" {
		t.Errorf("external_id = %v, want the mirror row's id with /1", op.ExternalID)
	}
	// The space is the account's, filled in by the write path (see
	// operation.insertSQL); stating it here would be a second copy of it.
	if op.SpaceID != uuid.Nil {
		t.Errorf("space_id = %s, want the zero uuid", op.SpaceID)
	}
	if op.TransferGroupID != nil || len(op.TransferLots) != 0 || op.SettledOn != nil || op.SplitRatio != nil {
		t.Errorf("a trade carried transfer/settlement/split fields: %+v", op)
	}
}

func TestProjectRowSell(t *testing.T) {
	row := mirrorRowFor(t, "sell.json")
	op := projectOne(t, row, resolvedShare())

	if op.Type != operation.TypeSell {
		t.Errorf("type = %s, want sell", op.Type)
	}
	if want := day(t, "2026-05-20"); !op.OccurredOn.Equal(want) {
		t.Errorf("occurred_on = %s, want 2026-05-20", op.OccurredOn.Format(time.RFC3339))
	}
	if op.AmountMinor != 3120000 {
		t.Errorf("amount_minor = %d, want 3120000", op.AmountMinor)
	}
	if op.FeeMinor != 936 {
		t.Errorf("fee_minor = %d, want 936", op.FeeMinor)
	}
	if op.Quantity == nil || op.Quantity.String() != "100" {
		t.Errorf("quantity = %v, want 100", op.Quantity)
	}
	if op.Price == nil || op.Price.String() != "312" {
		t.Errorf("price = %v, want 312", op.Price)
	}
}

// TestProjectRowPartialFillIsTheTradeNotTheOrder is the whole of #131 in one
// row, and it is a row the broker really sent.
//
// The two numbers are pinned as LITERALS, not derived from the fixture: an
// expectation computed from the same field the code reads would move with the
// mistake instead of catching it. 115 is what was sold and 190 is what was
// ordered; the payment agrees with the first and not with the second, which is
// the reason the journal must take that one.
func TestProjectRowPartialFillIsTheTradeNotTheOrder(t *testing.T) {
	row := mirrorRowFor(t, "sell_partially_filled.json")
	if row.Quantity != 190 || row.QuantityDone != 115 {
		t.Fatalf("the fixture stopped being the case under test: order %d, filled %d, want 190 and 115",
			row.Quantity, row.QuantityDone)
	}

	op := projectOne(t, row, resolvedShare())

	if op.Quantity == nil || op.Quantity.String() != "115" {
		t.Errorf("quantity = %v, want 115 — the bonds that were sold, not the 190 the order asked for", op.Quantity)
	}
	// The money is untouched by any of this: the payment is what the broker
	// paid for the part it executed, so it is the whole sum either way. What
	// the wrong count corrupts is the money PER UNIT, which is why the pair is
	// checked together.
	if op.AmountMinor != 12712100 {
		t.Errorf("amount_minor = %d, want 12712100", op.AmountMinor)
	}
	if op.FeeMinor != 4770 {
		t.Errorf("fee_minor = %d, want 4770", op.FeeMinor)
	}
}

// TestProjectRowTradeWithoutAFilledQuantityIsRefused pins the other half: when
// the broker says nothing about what was executed, the order's size is NOT
// borrowed to stand in for it. Borrowing it is the defect — silently, and by
// up to two and a half times.
func TestProjectRowTradeWithoutAFilledQuantityIsRefused(t *testing.T) {
	row := mirrorRowFor(t, "sell_without_fill.json")
	if row.Quantity != 190 || row.QuantityDone != 0 {
		t.Fatalf("the fixture stopped being the case under test: order %d, filled %d, want 190 and 0",
			row.Quantity, row.QuantityDone)
	}

	ops, _, refusal := ProjectRow(row, fixtureAccountID, resolvedShare(), nil)
	if refusal == nil {
		t.Fatalf("a sale of an unknown number of shares was projected into %d operations", len(ops))
	}
	if refusal.Reason != ReasonTradeWithoutFill {
		t.Errorf("reason = %q, want %q", refusal.Reason, ReasonTradeWithoutFill)
	}
	if len(ops) != 0 {
		t.Errorf("got %d operations alongside the refusal, want none", len(ops))
	}
	// The order's size belongs in the detail — it is what the owner will see
	// in the broker's own app — and nowhere else.
	if !strings.Contains(refusal.Detail, "190") {
		t.Errorf("detail = %q, want it to name the order of 190 units", refusal.Detail)
	}
}

// TestProjectRowSplitsOffACommissionChargedInAnotherCurrency pins the second
// leg: what makes it necessary is that one journal row holds one currency, and
// what makes it safe is that it carries no instrument (a fee in another
// currency attached to the same position makes the whole account unreadable —
// see TestProjectedOperationsFoldThroughTheEngine).
func TestProjectRowSplitsOffACommissionChargedInAnotherCurrency(t *testing.T) {
	row := mirrorRowFor(t, "buy_fee_in_another_currency.json")
	ops, _, refusal := ProjectRow(row, fixtureAccountID, resolvedShare(), nil)
	if refusal != nil {
		t.Fatalf("refused: %v", refusal)
	}
	if len(ops) != 2 {
		t.Fatalf("got %d operations, want 2", len(ops))
	}
	trade, fee := ops[0], ops[1]

	if trade.Type != operation.TypeBuy || trade.Currency != "USD" || trade.AmountMinor != -120050 {
		t.Errorf("trade = %s %s %d, want buy USD -120050", trade.Type, trade.Currency, trade.AmountMinor)
	}
	if trade.FeeMinor != 0 {
		t.Errorf("trade fee_minor = %d, want 0 — the commission is another currency's number", trade.FeeMinor)
	}
	if fee.Type != operation.TypeFee || fee.Currency != "RUB" || fee.AmountMinor != -9540 {
		t.Errorf("fee leg = %s %s %d, want fee RUB -9540", fee.Type, fee.Currency, fee.AmountMinor)
	}
	if fee.InstrumentID != nil {
		t.Errorf("fee leg names an instrument (%v); a fee in another currency must stay cash-level", fee.InstrumentID)
	}
	if fee.FeeMinor != 0 || fee.Quantity != nil || fee.Price != nil {
		t.Errorf("fee leg carried trade fields: %+v", fee)
	}
	if !trade.OccurredOn.Equal(fee.OccurredOn) || !trade.OccurredOn.Equal(day(t, "2026-04-02")) {
		t.Errorf("legs are dated %s and %s, want both 2026-04-02",
			trade.OccurredOn.Format("2006-01-02"), fee.OccurredOn.Format("2006-01-02"))
	}
	if trade.ExternalID == nil || *trade.ExternalID != "11111111-1111-4111-8111-111111111111/1" {
		t.Errorf("trade external_id = %v, want the row id with /1", trade.ExternalID)
	}
	if fee.ExternalID == nil || *fee.ExternalID != "11111111-1111-4111-8111-111111111111/2" {
		t.Errorf("fee external_id = %v, want the row id with /2", fee.ExternalID)
	}
	if !strings.Contains(fee.Note, "комиссия сделки, списанная в другой валюте") {
		t.Errorf("fee note = %q, want it to say the commission was charged in another currency", fee.Note)
	}
}

// TestProjectRowCurrencyTradeStaysUnparsed pins the deliberate departure from
// the plan's mapping table: a currency purchase does not become two conversion
// rows, because nothing in the mirror row names the currency that was bought.
// It refuses even when a caller passes a resolved instrument, since the
// resolver must never be asked for a currency in the first place.
//
// IT INSISTS ON currency_trade RATHER THAN ON "some refusal", and that is the
// whole point of the reason having its own code. With the branch deleted, a
// currency trade with nothing resolved still refuses — instrumentRefusal gets
// there by another road and says unsupported_type, the reason a futures trade
// gets — so a test that only asked for "a refusal", or for that shared code,
// would stay green with the rule gone. The two statements are different: a
// future is not accounted for at all, a currency conversion is not imported
// YET and for the reason projection.go names.
func TestProjectRowCurrencyTradeStaysUnparsed(t *testing.T) {
	row := mirrorRowFor(t, "currency_buy.json")
	for _, resolved := range []*Resolved{nil, resolvedShare()} {
		ops, _, refusal := ProjectRow(row, fixtureAccountID, resolved, nil)
		if len(ops) != 0 {
			t.Fatalf("got %d operations, want none", len(ops))
		}
		if refusal == nil {
			t.Fatal("a currency trade was projected instead of being refused")
		}
		if refusal.Reason != ReasonCurrencyTrade {
			t.Errorf("reason = %q, want currency_trade — not the code a kind of asset this program does not account for gets", refusal.Reason)
		}
	}
}

func TestProjectRowCashOperations(t *testing.T) {
	cases := []struct {
		fixture string
		// resolved is what the caller would hand in: a share for the rows
		// that name one, nothing for the rows that do not.
		resolved       *Resolved
		wantType       operation.Type
		wantAmount     int64
		wantDay        string
		wantInstrument bool
	}{
		{"input.json", nil, operation.TypeDeposit, 5000000, "2026-01-09", false},
		{"output.json", nil, operation.TypeWithdrawal, -1500000, "2026-02-11", false},
		{"dividend.json", resolvedShare(), operation.TypeDividend, 135075, "2026-06-05", true},
		{"coupon.json", resolvedShare(), operation.TypeCoupon, 4149, "2026-07-15", true},
		{"dividend_tax.json", resolvedShare(), operation.TypeTax, -17560, "2026-06-05", true},
		// 21:00Z is midnight in Moscow: the fee falls on the next day.
		{"service_fee.json", nil, operation.TypeFee, -29900, "2026-03-02", false},
		// Interest names a share in this fixture and still gets no
		// instrument: the engine refuses an interest row that carries one, so
		// the reference is ignored rather than recorded or refused.
		{"overnight.json", nil, operation.TypeInterest, 1234, "2026-04-10", false},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			row := mirrorRowFor(t, c.fixture)
			op := projectOne(t, row, c.resolved)
			if op.Type != c.wantType {
				t.Errorf("type = %s, want %s", op.Type, c.wantType)
			}
			if op.AmountMinor != c.wantAmount {
				t.Errorf("amount_minor = %d, want %d", op.AmountMinor, c.wantAmount)
			}
			if want := day(t, c.wantDay); !op.OccurredOn.Equal(want) {
				t.Errorf("occurred_on = %s, want %s", op.OccurredOn.Format("2006-01-02"), c.wantDay)
			}
			if (op.InstrumentID != nil) != c.wantInstrument {
				t.Errorf("instrument_id = %v, want present=%v", op.InstrumentID, c.wantInstrument)
			}
			if op.Quantity != nil || op.Price != nil || op.FeeMinor != 0 {
				t.Errorf("a cash operation carried quantity/price/fee: %+v", op)
			}
			if op.ExternalID == nil || *op.ExternalID != fixtureRowID.String()+"/1" {
				t.Errorf("external_id = %v, want the row id with /1", op.ExternalID)
			}
		})
	}
}

// TestProjectRowTaxRefundIsATaxThatGaveMoneyBACK. A broker's tax correction
// arrives with a positive amount and is an ordinary event — of the nine on the
// owner's own account seven are positive — so it is recorded as the tax it is,
// with the sign the broker sent.
//
// IT USED TO BE REFUSED, and the refusal cost seven real credits their place in
// the journal for as long as the account existed. What made the refusal look
// right was the alternatives: booked as a DEPOSIT it would leave the position's
// income understated by the refund for good and grow a top-up the owner never
// made; booked as INCOME it would inflate dividends that were never paid. The
// answer was neither — it was that a tax is folded into income by its SIGNED
// amount, so a refund restores exactly what the withholding took, and nothing
// downstream needed teaching at all.
//
// The hand-entry path still refuses a positive tax, and should: there the sign
// is somebody's typing rather than the broker's statement (see
// operation.validateImported).
func TestProjectRowTaxRefundIsATaxThatGaveMoneyBACK(t *testing.T) {
	row := mirrorRowFor(t, "tax_correction_refund.json")
	// Resolved, because the fixture's correction names a paper: an unresolved
	// instrument is refused for that, later and for its own reason.
	for _, resolved := range []*Resolved{resolvedShare()} {
		ops, _, refusal := ProjectRow(row, fixtureAccountID, resolved, nil)
		if refusal != nil {
			t.Fatalf("refused with %q — a correction that gives money back is money that moved", refusal.Reason)
		}
		if len(ops) != 1 {
			t.Fatalf("got %d operations, want exactly one", len(ops))
		}
		if ops[0].Type != operation.TypeTax {
			t.Errorf("type = %s, want tax: it is a tax, and it is the sign that differs", ops[0].Type)
		}
		if ops[0].AmountMinor <= 0 {
			t.Errorf("amount = %d, want it positive and unchanged — the sign is the broker's own statement about which way the money went", ops[0].AmountMinor)
		}
	}
}

// TestProjectRowTaxKeepsItsSignWhenItIsATax is the other half of the rule
// above: an ordinary tax stays a tax, so the refusal cannot be mistaken for
// "corrections are never projected".
func TestProjectRowTaxKeepsItsSignWhenItIsATax(t *testing.T) {
	row := mirrorRowFor(t, "tax_correction_refund.json")
	row.Payment = decimal.RequireFromString("-320")
	op := projectOne(t, row, resolvedShare())

	if op.Type != operation.TypeTax {
		t.Errorf("type = %s, want tax", op.Type)
	}
	if op.AmountMinor != -32000 {
		t.Errorf("amount_minor = %d, want -32000", op.AmountMinor)
	}
	if op.Note != "Корректировка налога" {
		t.Errorf("note = %q, want the broker's description alone", op.Note)
	}
	if op.InstrumentID == nil {
		t.Error("instrument_id = nil, want the resolved share — a tax is attributed to its position")
	}
}

// TestProjectRowTaxOfNothingIsHandedToTheJournal pins the sentence projectCash
// writes about a zero: it is not money given back, so calling it a refund would
// be a reason that is not the true one. It goes to the journal as a tax, and the
// journal's own refusal (which this projection does not pre-empt) is what the
// owner will read.
func TestProjectRowTaxOfNothingIsHandedToTheJournal(t *testing.T) {
	row := mirrorRowFor(t, "tax_correction_refund.json")
	row.Payment = decimal.Zero
	op := projectOne(t, row, resolvedShare())

	if op.Type != operation.TypeTax {
		t.Errorf("type = %s, want tax", op.Type)
	}
	if op.AmountMinor != 0 {
		t.Errorf("amount_minor = %d, want 0", op.AmountMinor)
	}
}

// TestProjectRowAmortizationCarriesNoQuantity pins that the broker's count of
// bonds is deliberately not written: the engine reads a quantity as units that
// moved, and an amortization moves none.
func TestProjectRowAmortizationCarriesNoQuantity(t *testing.T) {
	row := mirrorRowFor(t, "bond_repayment.json")
	op := projectOne(t, row, &Resolved{InstrumentID: fixtureInstrID, Type: instrument.TypeBond})

	if op.Type != operation.TypeAmortization {
		t.Errorf("type = %s, want amortization", op.Type)
	}
	if op.AmountMinor != 20000 {
		t.Errorf("amount_minor = %d, want 20000", op.AmountMinor)
	}
	if op.Quantity != nil {
		t.Errorf("quantity = %v, want none", op.Quantity)
	}
	if op.InstrumentID == nil {
		t.Error("instrument_id = nil, want the resolved bond")
	}
}

func TestProjectRowFullRedemptionIsItsOwnKindOfDisposal(t *testing.T) {
	row := mirrorRowFor(t, "bond_repayment_full.json")
	op := projectOne(t, row, &Resolved{InstrumentID: fixtureInstrID, Type: instrument.TypeBond})

	if op.Type != operation.TypeRedemption {
		t.Errorf("type = %s, want redemption — the bond matured, nobody sold it", op.Type)
	}
	if op.AmountMinor != 1000000 {
		t.Errorf("amount_minor = %d, want 1000000", op.AmountMinor)
	}
	if op.Quantity == nil || op.Quantity.String() != "10" {
		t.Errorf("quantity = %v, want 10", op.Quantity)
	}
	if op.Price == nil || op.Price.String() != "1000" {
		t.Errorf("price = %v, want 1000", op.Price)
	}
}

// TestProjectRowFullRedemptionKeepsItsCommission pins that a redemption's
// commission is carried exactly the way a trade's is — into FeeMinor when it is
// the row's own currency, into an entry of its own when it is not. A redemption
// that dropped it would make that money disappear from the journal AND from the
// unparsed list, which is the one thing this file forbids.
func TestProjectRowFullRedemptionKeepsItsCommission(t *testing.T) {
	bond := &Resolved{InstrumentID: fixtureInstrID, Type: instrument.TypeBond}

	t.Run("the row's own currency goes into fee_minor", func(t *testing.T) {
		op := projectOne(t, mirrorRowFor(t, "bond_repayment_full_with_fee.json"), bond)
		if op.Type != operation.TypeRedemption {
			t.Errorf("type = %s, want redemption", op.Type)
		}
		if op.AmountMinor != 1000000 {
			t.Errorf("amount_minor = %d, want 1000000", op.AmountMinor)
		}
		if op.FeeMinor != 300 {
			t.Errorf("fee_minor = %d, want 300 — the broker's 3 roubles", op.FeeMinor)
		}
	})

	t.Run("another currency becomes an entry of its own", func(t *testing.T) {
		row := mirrorRowFor(t, "bond_repayment_full_with_fee.json")
		row.CommissionCurrency = "USD"
		ops, _, refusal := ProjectRow(row, fixtureAccountID, bond, nil)
		if refusal != nil {
			t.Fatalf("refused: %v", refusal)
		}
		if len(ops) != 2 {
			t.Fatalf("got %d operations, want 2", len(ops))
		}
		sale, fee := ops[0], ops[1]
		if sale.FeeMinor != 0 {
			t.Errorf("sale fee_minor = %d, want 0 — the commission is another currency's number", sale.FeeMinor)
		}
		if fee.Type != operation.TypeFee || fee.Currency != "USD" || fee.AmountMinor != -300 {
			t.Errorf("fee leg = %s %s %d, want fee USD -300", fee.Type, fee.Currency, fee.AmountMinor)
		}
		if fee.InstrumentID != nil {
			t.Errorf("fee leg names an instrument (%v); a fee in another currency must stay cash-level", fee.InstrumentID)
		}
		if sale.ExternalID == nil || *sale.ExternalID != "11111111-1111-4111-8111-111111111111/1" {
			t.Errorf("sale external_id = %v, want /1", sale.ExternalID)
		}
		if fee.ExternalID == nil || *fee.ExternalID != "11111111-1111-4111-8111-111111111111/2" {
			t.Errorf("fee external_id = %v, want /2", fee.ExternalID)
		}
	})

	t.Run("a commission the broker gave back is refused, not flipped", func(t *testing.T) {
		row := mirrorRowFor(t, "bond_repayment_full_with_fee.json")
		back := decimal.RequireFromString("3")
		row.Commission = &back
		ops, _, refusal := ProjectRow(row, fixtureAccountID, bond, nil)
		if len(ops) != 0 {
			t.Fatalf("got %d operations, want none", len(ops))
		}
		if refusal == nil || refusal.Reason != ReasonCommissionRefund {
			t.Fatalf("refusal = %v, want commission_refund", refusal)
		}
	})
}

// TestProjectRowFullRedemptionWithoutQuantityAsksTheJournalForTheCount pins
// what the live run found: the broker reports a full redemption as a payment
// and nothing else. The row is READ — the money, the day, the security are all
// in it — and the one thing missing is the count, which is the position the
// account holds and therefore not a property of this row at all. So the sale is
// built without a quantity and the deferral says who owes it; the rebuild
// fills it in, and only the rebuild can (see closeRedemptions).
//
// The commission is in the same entry, on purpose: a redemption that waits for
// its count must not lose its fee on the way.
func TestProjectRowFullRedemptionWithoutQuantityAsksTheJournalForTheCount(t *testing.T) {
	row := mirrorRowFor(t, "bond_repayment_full_no_quantity.json")
	ops, deferred, refusal := ProjectRow(row, fixtureAccountID, &Resolved{InstrumentID: fixtureInstrID, Type: instrument.TypeBond}, nil)

	if refusal != nil {
		t.Fatalf("refused: %v — a redemption the broker priced in money alone is readable, it is only incomplete", refusal)
	}
	if len(ops) != 1 {
		t.Fatalf("got %d operations, want 1", len(ops))
	}
	if deferred != DeferredRedeemedQuantity {
		t.Errorf("deferred = %d, want DeferredRedeemedQuantity (%d)", deferred, DeferredRedeemedQuantity)
	}
	sale := ops[0]
	if sale.Type != operation.TypeRedemption {
		t.Errorf("type = %s, want redemption", sale.Type)
	}
	if sale.Quantity != nil {
		t.Errorf("quantity = %v, want none: a count invented here would be this program saying how many bonds the broker redeemed", sale.Quantity)
	}
	if sale.AmountMinor != 1000000 {
		t.Errorf("amount_minor = %d, want 1000000 — the broker's payment", sale.AmountMinor)
	}
	if sale.InstrumentID == nil || *sale.InstrumentID != fixtureInstrID {
		t.Errorf("instrument_id = %v, want the resolved bond", sale.InstrumentID)
	}
	if sale.ExternalID == nil || *sale.ExternalID != "11111111-1111-4111-8111-111111111111/1" {
		t.Errorf("external_id = %v, want /1", sale.ExternalID)
	}
}

// TestProjectRowFullRedemptionWithAQuantityIsComplete is the other half: a
// redemption that DOES name a count is a finished sale and asks the journal for
// nothing. Without this, a rule that deferred every redemption would still pass
// the test above.
func TestProjectRowFullRedemptionWithAQuantityIsComplete(t *testing.T) {
	row := mirrorRowFor(t, "bond_repayment_full.json")
	ops, deferred, refusal := ProjectRow(row, fixtureAccountID, &Resolved{InstrumentID: fixtureInstrID, Type: instrument.TypeBond}, nil)

	if refusal != nil {
		t.Fatalf("refused: %v", refusal)
	}
	if len(ops) != 1 {
		t.Fatalf("got %d operations, want 1", len(ops))
	}
	if deferred != DeferredNothing {
		t.Errorf("deferred = %d, want DeferredNothing (%d)", deferred, DeferredNothing)
	}
	if ops[0].Quantity == nil || ops[0].Quantity.String() != "10" {
		t.Errorf("quantity = %v, want 10 — the broker's own count", ops[0].Quantity)
	}
}

// TestProjectRowDividendToCardIsPaidAndWithdrawnTheSameDay pins decision 5:
// one entry would either lose the income or grow a cash balance that never
// grew.
func TestProjectRowDividendToCardIsPaidAndWithdrawnTheSameDay(t *testing.T) {
	row := mirrorRowFor(t, "div_ext.json")
	ops, _, refusal := ProjectRow(row, fixtureAccountID, resolvedShare(), nil)
	if refusal != nil {
		t.Fatalf("refused: %v", refusal)
	}
	if len(ops) != 2 {
		t.Fatalf("got %d operations, want 2", len(ops))
	}
	income, out := ops[0], ops[1]

	if income.Type != operation.TypeDividend || income.AmountMinor != 98000 {
		t.Errorf("income leg = %s %d, want dividend 98000", income.Type, income.AmountMinor)
	}
	if out.Type != operation.TypeWithdrawal || out.AmountMinor != -98000 {
		t.Errorf("outgoing leg = %s %d, want withdrawal -98000", out.Type, out.AmountMinor)
	}
	if !income.OccurredOn.Equal(day(t, "2026-06-20")) || !out.OccurredOn.Equal(income.OccurredOn) {
		t.Errorf("legs are dated %s and %s, want both 2026-06-20",
			income.OccurredOn.Format("2006-01-02"), out.OccurredOn.Format("2006-01-02"))
	}
	if income.InstrumentID == nil {
		t.Error("the dividend leg carries no instrument")
	}
	if out.InstrumentID != nil {
		t.Errorf("the withdrawal leg names an instrument (%v); the engine refuses one", out.InstrumentID)
	}
	for i, op := range ops {
		if !strings.Contains(op.Note, "выплата на карту, минуя брокерский счёт") {
			t.Errorf("leg %d note = %q, want it to say the money bypassed the account", i+1, op.Note)
		}
	}
	if income.ExternalID == nil || *income.ExternalID != "11111111-1111-4111-8111-111111111111/1" {
		t.Errorf("income external_id = %v, want /1", income.ExternalID)
	}
	if out.ExternalID == nil || *out.ExternalID != "11111111-1111-4111-8111-111111111111/2" {
		t.Errorf("withdrawal external_id = %v, want /2", out.ExternalID)
	}
}

// TestProjectRowIncomingSecuritiesSayTheirBasisIsUnknown pins decision 9: the
// broker does not report what shares cost at the broker they came from, so the
// basis is zero and the note says why — on THAT case only.
func TestProjectRowIncomingSecuritiesSayTheirBasisIsUnknown(t *testing.T) {
	row := mirrorRowFor(t, "input_securities.json")
	op := projectOne(t, row, resolvedShare())

	if op.Type != operation.TypeTransferIn {
		t.Errorf("type = %s, want transfer_in", op.Type)
	}
	if op.AmountMinor != 0 {
		t.Errorf("amount_minor = %d, want 0", op.AmountMinor)
	}
	if op.Quantity == nil || op.Quantity.String() != "40" {
		t.Errorf("quantity = %v, want 40", op.Quantity)
	}
	if op.Note != "Перевод бумаг от другого брокера — стоимость приобретения неизвестна: брокер её не передаёт" {
		t.Errorf("note = %q, want the description plus the unknown-basis mark", op.Note)
	}
	if op.InstrumentID == nil {
		t.Error("instrument_id = nil, want the resolved share")
	}
	if op.TransferGroupID != nil || len(op.TransferLots) != 0 {
		t.Errorf("a leg arrived already paired or already carrying a parcel: %+v", op)
	}
}

func TestProjectRowOutgoingSecurities(t *testing.T) {
	row := mirrorRowFor(t, "output_securities.json")
	op := projectOne(t, row, resolvedShare())

	if op.Type != operation.TypeTransferOut {
		t.Errorf("type = %s, want transfer_out", op.Type)
	}
	if op.AmountMinor != 0 {
		t.Errorf("amount_minor = %d, want 0 — the basis is released from the journal, not supplied", op.AmountMinor)
	}
	if op.Quantity == nil || op.Quantity.String() != "40" {
		t.Errorf("quantity = %v, want 40", op.Quantity)
	}
	// The unknown-basis mark belongs to shares arriving from another broker
	// and to nothing else: a departing leg's basis is known exactly.
	if op.Note != "Перевод бумаг другому брокеру" {
		t.Errorf("note = %q, want the broker's description alone", op.Note)
	}
}

// TestProjectRowTransferBetweenOwnAccountsReadsItsDirectionFromTheQuantity
// pins the assumption named in projectSecuritiesTransfer: the operation type
// appears on both accounts, so only the sign says which side this row is.
func TestProjectRowTransferBetweenOwnAccountsReadsItsDirectionFromTheQuantity(t *testing.T) {
	in := projectOne(t, mirrorRowFor(t, "trans_bs_bs_in.json"), resolvedShare())
	if in.Type != operation.TypeTransferIn {
		t.Errorf("positive quantity gave %s, want transfer_in", in.Type)
	}
	if in.Quantity == nil || in.Quantity.String() != "5" {
		t.Errorf("quantity = %v, want 5", in.Quantity)
	}
	// No unknown-basis note here: this leg may yet be paired with its other
	// half, and then the basis is known exactly.
	if in.Note != "Перевод бумаг между счетами" {
		t.Errorf("note = %q, want the broker's description alone", in.Note)
	}

	out := projectOne(t, mirrorRowFor(t, "trans_bs_bs_out.json"), resolvedShare())
	if out.Type != operation.TypeTransferOut {
		t.Errorf("negative quantity gave %s, want transfer_out", out.Type)
	}
	if out.Quantity == nil || out.Quantity.String() != "5" {
		t.Errorf("quantity = %v, want 5 — the journal's quantity is a magnitude", out.Quantity)
	}
}

// TestProjectRowTransferOfNothingIsRefused pins the reason as well as the
// refusal: a zero is perfectly representable, and what is missing is the
// DIRECTION — the only thing that says which side of the move this row is.
func TestProjectRowTransferOfNothingIsRefused(t *testing.T) {
	row := mirrorRowFor(t, "trans_bs_bs_in.json")
	row.Quantity = 0
	ops, _, refusal := ProjectRow(row, fixtureAccountID, resolvedShare(), nil)

	if len(ops) != 0 {
		t.Fatalf("got %d operations, want none", len(ops))
	}
	if refusal == nil {
		t.Fatal("a transfer of zero units was projected instead of being refused")
	}
	if refusal.Reason != ReasonTransferDirectionUnknown {
		t.Errorf("reason = %q, want transfer_direction_unknown — a zero is representable, a direction is what is missing", refusal.Reason)
	}
}

// TestProjectRowOneSidedTransferOfNothingNamesTheMissingNumber is the owner's
// own row, and the reason it needs a name of its own.
//
// The broker moved 0.24 of a share of Warner Bros. Discovery in from another
// depository. EVERY quantity field it sends is an integer — quantity,
// quantityDone and quantityRest — so all three came back "0", and the real
// number survives in the Russian prose of the description and nowhere else.
//
// Built anyway, such a row was refused by the JOURNAL with "transfer_in
// requires positive quantity": true of our rule, and silent about what
// happened. The reader could not tell a program bug from a number the broker
// never sent — which is the difference between something to report and a line
// to enter by hand.
//
// A one-sided transfer, deliberately: the two-sided kind reads its DIRECTION
// from the same sign and refuses a zero earlier, for a reason of its own (see
// the test above), and that refusal must keep its own wording.
func TestProjectRowOneSidedTransferOfNothingNamesTheMissingNumber(t *testing.T) {
	row := mirrorRowFor(t, "input_securities.json")
	row.Quantity = 0
	row.Description = "Завод 0.24 акций Warner Bros. Discovery из другого депозитария"
	ops, _, refusal := ProjectRow(row, fixtureAccountID, resolvedShare(), nil)

	if len(ops) != 0 {
		t.Fatalf("got %d operations, want none — the journal refuses this one anyway, and the refusal it gives names our rule instead of the broker's silence", len(ops))
	}
	if refusal == nil {
		t.Fatal("a transfer of no units was projected instead of being refused")
	}
	switch refusal.Reason {
	case ReasonTransferDirectionUnknown:
		t.Errorf("reason = %q — the direction of a one-sided transfer is in its TYPE, not in a sign, and nothing about it is unknown here", refusal.Reason)
	case ReasonTransferWithoutQuantity:
	default:
		t.Errorf("reason = %q, want transfer_without_quantity", refusal.Reason)
	}
	// The description is carried into the detail, because it is the only place
	// the real number exists — a reader retyping the line by hand needs it.
	if !strings.Contains(refusal.Detail, "0.24") {
		t.Errorf("detail = %q, want the broker's own description in it: the figure is in that prose and nowhere else", refusal.Detail)
	}
}

// TestProjectRowShapeWithNoBranchRefusesInsteadOfVanishing pins the switch's
// default arm — the one case in this file that cannot be reached through any
// input, because it is reached through a change to the code instead: a shape
// added to the mapping table tomorrow with no branch built for it.
//
// It is checked by putting exactly that in the table for the length of this
// test. Without the default arm the row projects to nothing at all and says
// nothing about why, which is the silent drop the file's own heading forbids.
// TestBrokerOpTypesPairShapeWithDirection checks the mapping table's one
// cross-field invariant: a transfer kind belongs to a transfer shape and to no
// other, in both directions. A securities move without a kind would fall
// through projectSecuritiesTransfer's switch to transfer_in and file a
// departure as an arrival; a kind on any other shape would be a direction
// nothing reads, quietly suggesting the row was thought about as a transfer.
func TestBrokerOpTypesPairShapeWithDirection(t *testing.T) {
	moves := 0
	for opType, r := range brokerOpTypes {
		isMove := r.how == asSecuritiesTransfer
		if isMove {
			moves++
		}
		if isMove && r.transfer == transferNone {
			t.Errorf("%s is a securities move with no direction", opType)
		}
		if !isMove && r.transfer != transferNone {
			t.Errorf("%s is not a securities move but carries direction %d", opType, r.transfer)
		}
	}
	// The four the broker has: in, out, and the two between the owner's own
	// accounts. Typed out so that a fifth added without a direction, or a
	// fourth deleted, fails here.
	if moves != 4 {
		t.Errorf("%d securities-move types in the table, want 4", moves)
	}
}

func TestProjectRowShapeWithNoBranchRefusesInsteadOfVanishing(t *testing.T) {
	const opType = "OPERATION_TYPE_A_SHAPE_ADDED_WITHOUT_A_BRANCH"
	if _, taken := brokerOpTypes[opType]; taken {
		t.Fatalf("%s is a real broker type; pick another name for this test", opType)
	}
	brokerOpTypes[opType] = rule{how: shape(200), journal: operation.TypeDeposit}
	defer delete(brokerOpTypes, opType)

	row := mirrorRowFor(t, "input.json")
	row.OpType = opType
	ops, _, refusal := ProjectRow(row, fixtureAccountID, nil, nil)

	if len(ops) != 0 {
		t.Fatalf("got %d operations, want none", len(ops))
	}
	if refusal == nil {
		t.Fatal("a shape with no branch produced nothing and no reason — the silent drop")
	}
	if refusal.Reason != ReasonProjectionIncomplete {
		t.Errorf("reason = %q, want projection_incomplete", refusal.Reason)
	}
}

// TestProjectRowBrokerFeeIsBuiltAndHeld. A commission the broker charged as an
// operation of its own is now BUILT here and marked as owing a verdict, rather
// than dropped on the spot as a duplicate.
//
// The reason is a single row out of the owner's 311: one purchase carries no
// commission field at all, so there the separate operation is the only record
// of that money and dropping it lost the charge outright. Which case a fee is
// in is a question about another row, and this function sees one row — hence
// the deferral rather than an answer (see DeferredBrokerFeeVerdict and
// Rebuilder.settleBrokerFees).
func TestProjectRowBrokerFeeIsBuiltAndHeld(t *testing.T) {
	ops, deferred, refusal := ProjectRow(mirrorRowFor(t, "broker_fee.json"), fixtureAccountID, nil, nil)
	if refusal != nil {
		t.Fatalf("refused: %v — a broker fee is understood, it is simply not always kept", refusal)
	}
	if len(ops) != 1 {
		t.Fatalf("got %d operations, want 1", len(ops))
	}
	if ops[0].Type != operation.TypeFee {
		t.Errorf("type = %s, want fee", ops[0].Type)
	}
	if deferred != DeferredBrokerFeeVerdict {
		t.Errorf("deferred = %v, want DeferredBrokerFeeVerdict — the entry cannot be judged from its own row", deferred)
	}
}

func TestProjectRowUnknownTypesStayVisible(t *testing.T) {
	// A fixture for the documented futures settlement, and four types typed
	// out here: the whole repo-tax family, the expirations, the enum's own
	// "unspecified", and a value the broker has not invented yet.
	row := mirrorRowFor(t, "delivery_buy.json")
	ops, _, refusal := ProjectRow(row, fixtureAccountID, nil, nil)
	if len(ops) != 0 || refusal == nil || refusal.Reason != ReasonUnsupportedType {
		t.Fatalf("DELIVERY_BUY: got %d operations, refusal %v; want none and unsupported_type", len(ops), refusal)
	}

	for _, opType := range []string{
		"OPERATION_TYPE_TAX_REPO",
		"OPERATION_TYPE_TAX_REPO_PROGRESSIVE",
		"OPERATION_TYPE_OPTION_EXPIRATION",
		"OPERATION_TYPE_ACCRUING_VARMARGIN",
		"OPERATION_TYPE_DIVIDEND_TRANSFER",
		"OPERATION_TYPE_UNSPECIFIED",
		"OPERATION_TYPE_SOMETHING_THE_BROKER_ADDS_TOMORROW",
		"",
	} {
		row := mirrorRowFor(t, "input.json")
		row.OpType = opType
		ops, _, refusal := ProjectRow(row, fixtureAccountID, nil, nil)
		if len(ops) != 0 {
			t.Errorf("%s: got %d operations, want none", opType, len(ops))
		}
		if refusal == nil || refusal.Reason != ReasonUnsupportedType {
			t.Errorf("%s: refusal = %v, want unsupported_type", opType, refusal)
		}
	}
}

// TestProjectRowSkipsWhatDidNotHappen pins the one place where "no operations
// and no refusal" is the right answer: an operation that was cancelled, is
// still in progress, or that the broker has stopped reporting. Calling those
// unparsed would fill the owner's list of things this program could not read
// with orders that simply did not happen.
func TestProjectRowSkipsWhatDidNotHappen(t *testing.T) {
	cancelled := mirrorRowFor(t, "cancelled_buy.json")
	ops, _, refusal := ProjectRow(cancelled, fixtureAccountID, resolvedShare(), nil)
	if len(ops) != 0 || refusal != nil {
		t.Errorf("cancelled: got %d operations and refusal %v, want none and none", len(ops), refusal)
	}

	inProgress := mirrorRowFor(t, "buy.json")
	inProgress.State = "OPERATION_STATE_PROGRESS"
	ops, _, refusal = ProjectRow(inProgress, fixtureAccountID, resolvedShare(), nil)
	if len(ops) != 0 || refusal != nil {
		t.Errorf("in progress: got %d operations and refusal %v, want none and none", len(ops), refusal)
	}

	gone := mirrorRowFor(t, "buy.json")
	at := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	gone.DisappearedAt = &at
	ops, _, refusal = ProjectRow(gone, fixtureAccountID, resolvedShare(), nil)
	if len(ops) != 0 || refusal != nil {
		t.Errorf("disappeared: got %d operations and refusal %v, want none and none", len(ops), refusal)
	}
}

func TestProjectRowRefusesAnAmountFinerThanAMinorUnit(t *testing.T) {
	row := mirrorRowFor(t, "fractional_payment.json")
	ops, _, refusal := ProjectRow(row, fixtureAccountID, nil, nil)

	if len(ops) != 0 {
		t.Fatalf("got %d operations, want none — 10.123456789 is not a sum this journal holds", len(ops))
	}
	if refusal == nil || refusal.Reason != ReasonUnrepresentableAmount {
		t.Fatalf("refusal = %v, want unrepresentable_amount", refusal)
	}
}

func TestProjectRowRefusesAnAmountBeyondTheBound(t *testing.T) {
	row := mirrorRowFor(t, "input.json")
	row.Payment = decimal.RequireFromString("10000000000000.01")
	ops, _, refusal := ProjectRow(row, fixtureAccountID, nil, nil)

	if len(ops) != 0 {
		t.Fatalf("got %d operations, want none", len(ops))
	}
	if refusal == nil || refusal.Reason != ReasonAmountOutOfBounds {
		t.Fatalf("refusal = %v, want amount_out_of_bounds", refusal)
	}
}

// TestProjectRowRefusesACommissionItCannotExpress pins that the commission is
// converted with the same care as the payment: a trade whose commission is
// finer than a kopeck is refused rather than recorded with the fee rounded.
func TestProjectRowRefusesACommissionItCannotExpress(t *testing.T) {
	row := mirrorRowFor(t, "buy.json")
	fraction := decimal.RequireFromString("-8.255")
	row.Commission = &fraction
	ops, _, refusal := ProjectRow(row, fixtureAccountID, resolvedShare(), nil)

	if len(ops) != 0 {
		t.Fatalf("got %d operations, want none", len(ops))
	}
	if refusal == nil || refusal.Reason != ReasonUnrepresentableAmount {
		t.Fatalf("refusal = %v, want unrepresentable_amount", refusal)
	}
}

// TestProjectRowCommissionSign pins that the SIGN of a commission is read
// rather than discarded.
//
// The magnitude is what FeeMinor holds — the journal's own rule — so a
// commission the broker sent back, i.e. positive, would be recorded as a
// commission charged: the owner would be shown a fee where a refund happened,
// wrong by twice the money and with nothing anywhere saying so. It refuses with
// a reason of its own instead. A zero is an ordinary trade with no commission.
func TestProjectRowCommissionSign(t *testing.T) {
	t.Run("money leaving is the fee", func(t *testing.T) {
		op := projectOne(t, mirrorRowFor(t, "buy.json"), resolvedShare())
		if op.FeeMinor != 825 {
			t.Errorf("fee_minor = %d, want 825", op.FeeMinor)
		}
	})

	t.Run("money coming back is refused", func(t *testing.T) {
		row := mirrorRowFor(t, "buy.json")
		back := decimal.RequireFromString("8.25")
		row.Commission = &back
		ops, _, refusal := ProjectRow(row, fixtureAccountID, resolvedShare(), nil)
		if len(ops) != 0 {
			t.Fatalf("got %d operations, want none — a returned commission is not a charge", len(ops))
		}
		if refusal == nil || refusal.Reason != ReasonCommissionRefund {
			t.Fatalf("refusal = %v, want commission_refund", refusal)
		}
	})

	t.Run("money coming back in another currency is refused too", func(t *testing.T) {
		row := mirrorRowFor(t, "buy_fee_in_another_currency.json")
		back := decimal.RequireFromString("95.40")
		row.Commission = &back
		ops, _, refusal := ProjectRow(row, fixtureAccountID, resolvedShare(), nil)
		if len(ops) != 0 {
			t.Fatalf("got %d operations, want none", len(ops))
		}
		if refusal == nil || refusal.Reason != ReasonCommissionRefund {
			t.Fatalf("refusal = %v, want commission_refund", refusal)
		}
	})

	t.Run("no commission at all is an ordinary trade", func(t *testing.T) {
		row := mirrorRowFor(t, "buy.json")
		zero := decimal.Zero
		row.Commission = &zero
		op := projectOne(t, row, resolvedShare())
		if op.FeeMinor != 0 {
			t.Errorf("fee_minor = %d, want 0", op.FeeMinor)
		}
		if op.AmountMinor != -2750000 {
			t.Errorf("amount_minor = %d, want -2750000 — a zero commission changes nothing else", op.AmountMinor)
		}
	})
}

func TestProjectRowInstrumentRules(t *testing.T) {
	t.Run("a trade whose security was not resolved is refused", func(t *testing.T) {
		ops, _, refusal := ProjectRow(mirrorRowFor(t, "buy.json"), fixtureAccountID, nil, nil)
		if len(ops) != 0 {
			t.Fatalf("got %d operations, want none", len(ops))
		}
		if refusal == nil || refusal.Reason != ReasonInstrumentUnresolved {
			t.Fatalf("refusal = %v, want instrument_unresolved", refusal)
		}
	})

	t.Run("a trade in a kind of asset this program does not account for says so", func(t *testing.T) {
		row := mirrorRowFor(t, "buy.json")
		row.InstrumentType = "futures"
		ops, _, refusal := ProjectRow(row, fixtureAccountID, nil, nil)
		if len(ops) != 0 {
			t.Fatalf("got %d operations, want none", len(ops))
		}
		if refusal == nil || refusal.Reason != ReasonUnsupportedType {
			t.Fatalf("refusal = %v, want unsupported_type — the asset kind, not the matching, is what failed", refusal)
		}
	})

	t.Run("income on an unmatched security is refused rather than left unattributed", func(t *testing.T) {
		ops, _, refusal := ProjectRow(mirrorRowFor(t, "dividend.json"), fixtureAccountID, nil, nil)
		if len(ops) != 0 {
			t.Fatalf("got %d operations, want none", len(ops))
		}
		if refusal == nil || refusal.Reason != ReasonInstrumentUnresolved {
			t.Fatalf("refusal = %v, want instrument_unresolved", refusal)
		}
	})

	t.Run("income the broker attached to no security at all is recorded at the cash level", func(t *testing.T) {
		row := mirrorRowFor(t, "dividend.json")
		row.InstrumentUID, row.FIGI, row.InstrumentType = "", "", ""
		op := projectOne(t, row, nil)
		if op.Type != operation.TypeDividend || op.AmountMinor != 135075 {
			t.Errorf("got %s %d, want dividend 135075", op.Type, op.AmountMinor)
		}
		if op.InstrumentID != nil {
			t.Errorf("instrument_id = %v, want none", op.InstrumentID)
		}
	})
}

func TestMinorFromDecimal(t *testing.T) {
	cases := []struct {
		in         string
		want       int64
		wantReason UnparsedReason // empty: no refusal
	}{
		{in: "0", want: 0},
		{in: "0.01", want: 1},
		{in: "-0.20", want: -20},
		{in: "12.34", want: 1234},
		{in: "-27500", want: -2750000},
		{in: "8.25", want: 825},
		// The bound itself is admissible; a kopeck past it is not. The
		// journal refuses only what is strictly beyond ±10^15 minor units.
		{in: "10000000000000", want: 1000000000000000},
		{in: "-10000000000000", want: -1000000000000000},
		{in: "10000000000000.01", wantReason: ReasonAmountOutOfBounds},
		{in: "-10000000000000.01", wantReason: ReasonAmountOutOfBounds},
		{in: "100000000000000000000", wantReason: ReasonAmountOutOfBounds},
		{in: "0.001", wantReason: ReasonUnrepresentableAmount},
		{in: "-0.005", wantReason: ReasonUnrepresentableAmount},
		{in: "10.123456789", wantReason: ReasonUnrepresentableAmount},
		// A third of a kopeck, as a decimal can hold it.
		{in: "0.003333333", wantReason: ReasonUnrepresentableAmount},
		// Both wrong at once: the shape is reported, not the size.
		{in: "10000000000000.001", wantReason: ReasonUnrepresentableAmount},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := MinorFromDecimal(decimal.RequireFromString(c.in))
			if c.wantReason == "" {
				if err != nil {
					t.Fatalf("MinorFromDecimal(%s) refused: %v", c.in, err)
				}
				if got != c.want {
					t.Fatalf("MinorFromDecimal(%s) = %d, want %d", c.in, got, c.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("MinorFromDecimal(%s) = %d, want a refusal", c.in, got)
			}
			refusal, ok := err.(*UnparsedError)
			if !ok {
				t.Fatalf("MinorFromDecimal(%s) returned %T, want *UnparsedError", c.in, err)
			}
			if refusal.Reason != c.wantReason {
				t.Fatalf("MinorFromDecimal(%s) reason = %q, want %q", c.in, refusal.Reason, c.wantReason)
			}
			if got != 0 {
				t.Fatalf("MinorFromDecimal(%s) returned %d alongside a refusal, want 0", c.in, got)
			}
		})
	}
}

// TestMskDay is the day rule on its own, including the two boundaries a zone
// mistake moves: 21:00Z, which is midnight in Moscow, and the second before
// it. The expected days are typed out — not computed from the offset the
// implementation uses — so a wrong offset cannot drag the expectation along
// with it.
func TestMskDay(t *testing.T) {
	cases := []struct{ instant, wantDay string }{
		{"2026-03-14T21:30:00Z", "2026-03-15"},
		{"2026-03-14T21:00:00Z", "2026-03-15"},
		{"2026-03-14T20:59:59Z", "2026-03-14"},
		{"2026-03-15T00:00:00Z", "2026-03-15"},
		{"2026-03-15T20:59:59.999999999Z", "2026-03-15"},
		{"2025-12-31T21:00:00Z", "2026-01-01"},
		// The same instant as the first case, sent in another zone: the day
		// is Moscow's whatever the sender's clock said.
		{"2026-03-15T02:30:00+05:00", "2026-03-15"},
	}
	for _, c := range cases {
		t.Run(c.instant, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339Nano, c.instant)
			if err != nil {
				t.Fatalf("parse %q: %v", c.instant, err)
			}
			got := mskDay(at)
			if want := day(t, c.wantDay); !got.Equal(want) {
				t.Fatalf("mskDay(%s) = %s, want %s", c.instant, got.Format(time.RFC3339), c.wantDay)
			}
			if h, m, s := got.UTC().Clock(); h != 0 || m != 0 || s != 0 {
				t.Fatalf("mskDay(%s) = %s, want midnight UTC — the shape a DATE column reads back",
					c.instant, got.Format(time.RFC3339))
			}
		})
	}
}

// TestAcceptsInstrumentAgreesWithTheEngine is what keeps acceptsInstrument
// from drifting away from portfolio.Compute, which is the authority on the
// question. It asks the engine directly, type by type, with an operation that
// is otherwise well-formed, and compares the answers.
func TestAcceptsInstrumentAgreesWithTheEngine(t *testing.T) {
	instrumentID := fixtureInstrID
	on := day(t, "2026-03-15")
	qty := decimal.RequireFromString("1")
	ratio := decimal.RequireFromString("2")

	// Every candidate is folded after a purchase of ten units, so that the
	// types which consume a position have one to consume.
	opening := operation.Operation{
		AccountID: fixtureAccountID, InstrumentID: &instrumentID, Type: operation.TypeBuy,
		OccurredOn: on, Quantity: decimalPtr("10"), AmountMinor: -100000, Currency: "RUB",
	}

	candidates := map[operation.Type]operation.Operation{
		operation.TypeBuy:          {Quantity: &qty, AmountMinor: -10000},
		operation.TypeSell:         {Quantity: &qty, AmountMinor: 10000},
		operation.TypeDeposit:      {AmountMinor: 10000},
		operation.TypeWithdrawal:   {AmountMinor: -10000},
		operation.TypeDividend:     {AmountMinor: 10000},
		operation.TypeCoupon:       {AmountMinor: 10000},
		operation.TypeAmortization: {AmountMinor: 10000},
		operation.TypeFee:          {AmountMinor: -10000},
		operation.TypeTax:          {AmountMinor: -10000},
		operation.TypeTransferIn:   {Quantity: &qty, AmountMinor: 0},
		operation.TypeTransferOut:  {Quantity: &qty, AmountMinor: 0},
		operation.TypeSplit:        {SplitRatio: &ratio},
		operation.TypeInterest:     {AmountMinor: 10000},
		operation.TypeConversion:   {AmountMinor: -10000},
	}
	// Fourteen is every type the journal has (portfolio.validTypes and the
	// operations table's own CHECK). The count and the validity are both
	// checked so that a type added to the journal tomorrow fails here rather
	// than quietly going unasked, and so that a typo cannot pass for one.
	if len(candidates) != 14 {
		t.Fatalf("this table holds %d types; the journal has 14", len(candidates))
	}
	for typ := range candidates {
		if !typ.Valid() {
			t.Fatalf("%q is not a journal type", typ)
		}
	}

	for typ, tpl := range candidates {
		t.Run(string(typ), func(t *testing.T) {
			op := tpl
			op.AccountID, op.InstrumentID, op.Type, op.OccurredOn, op.Currency = fixtureAccountID, &instrumentID, typ, on, "RUB"
			_, err := portfolio.Compute([]operation.Operation{opening, op})
			engineTakesIt := err == nil
			if engineTakesIt != acceptsInstrument(typ) {
				t.Fatalf("acceptsInstrument(%s) = %v, but the engine %s such a row (err=%v)",
					typ, acceptsInstrument(typ), map[bool]string{true: "folds", false: "refuses"}[engineTakesIt], err)
			}
		})
	}
}

// TestProjectedOperationsFoldThroughTheEngine checks the projection's output
// by VALUE rather than by shape: a purchase, its sale, a dividend, and a trade
// in another currency whose commission was charged in this one all go into the
// engine together, and the position that comes out is the one the broker's own
// numbers describe.
//
// It is the test that would fail if the foreign-currency commission leg ever
// carried an instrument: the engine holds a position's cost and its fees in one
// currency — income is the one figure exempt from that, and a commission is not
// income — so such a leg makes the whole account unreadable rather than
// recording a fee.
func TestProjectedOperationsFoldThroughTheEngine(t *testing.T) {
	rub := resolvedShare()
	usd := &Resolved{InstrumentID: otherInstrID, Type: instrument.TypeShare}

	var journal []operation.Operation
	for _, c := range []struct {
		fixture  string
		resolved *Resolved
	}{
		{"buy.json", rub},
		{"dividend.json", rub},
		{"sell.json", rub},
		{"buy_fee_in_another_currency.json", usd},
	} {
		ops, _, refusal := ProjectRow(mirrorRowFor(t, c.fixture), fixtureAccountID, c.resolved, nil)
		if refusal != nil {
			t.Fatalf("%s: refused: %v", c.fixture, refusal)
		}
		journal = append(journal, ops...)
	}
	if len(journal) != 5 {
		t.Fatalf("built %d journal entries, want 5", len(journal))
	}

	positions, err := portfolio.Compute(journal)
	if err != nil {
		t.Fatalf("the engine refused what the projection built: %v", err)
	}

	rubPos := positions[fixtureInstrID]
	if rubPos == nil {
		t.Fatal("no position for the rouble share")
	}
	if rubPos.Quantity.String() != "0" {
		t.Errorf("quantity = %s, want 0 — a hundred bought and a hundred sold", rubPos.Quantity)
	}
	if rubPos.FeesMinorIn(rubPos.Currency) != 1761 {
		t.Errorf("fees_minor = %d, want 1761 — 8.25 on the purchase and 9.36 on the sale", rubPos.FeesMinorIn(rubPos.Currency))
	}
	if got := rubPos.IncomeMinorIn("RUB"); got != 135075 {
		t.Errorf("income in RUB = %d, want 135075 — the gross dividend", got)
	}
	if rubPos.Currency != "RUB" {
		t.Errorf("currency = %q, want RUB", rubPos.Currency)
	}

	usdPos := positions[otherInstrID]
	if usdPos == nil {
		t.Fatal("no position for the dollar share")
	}
	if usdPos.Currency != "USD" {
		t.Errorf("currency = %q, want USD", usdPos.Currency)
	}
	if usdPos.Quantity.String() != "10" {
		t.Errorf("quantity = %s, want 10", usdPos.Quantity)
	}
	// Basis is the payment; the commission was charged in another currency
	// and is a cash-level fee of its own, so it is NOT in this cost.
	if usdPos.CostMinor != 120050 {
		t.Errorf("cost_minor = %d, want 120050", usdPos.CostMinor)
	}
	if usdPos.FeesMinorIn(usdPos.Currency) != 0 {
		t.Errorf("fees_minor = %d, want 0 — the commission is another currency's money", usdPos.FeesMinorIn(usdPos.Currency))
	}
}

// TestJournalQuantity pins what the quantization does, and says plainly what
// this test does NOT do.
//
// IT DOES NOT REACH THE REFUSAL, AND NOTHING CAN. journalQuantity takes an
// int64, and truncating a whole number to ten decimal places returns the same
// whole number for every value the type holds, the extremes included — so
// `units > 0 && !q.IsPositive()` has no input that satisfies it, and deleting
// the refusal would leave this file green. That is a statement about the
// parameter's type rather than about the test's thoroughness, which is why it
// is written here instead of being papered over with a case that only looks
// like it exercises the branch. Why the refusal is nonetheless kept is in
// journalQuantity's own note; ReasonUnrepresentableQty is a live code because
// it is what a positive quantity truncated to nothing WOULD be called.
//
// The extremes are typed out rather than computed so that a change of scale
// (portfolio.QuantityScale) cannot drag the expectation along with it.
func TestJournalQuantity(t *testing.T) {
	cases := []struct {
		units int64
		want  string
	}{
		{100, "100"},
		{1, "1"},
		{0, "0"},
		{-5, "-5"},
		{9223372036854775807, "9223372036854775807"},
		{-9223372036854775808, "-9223372036854775808"},
	}
	for _, c := range cases {
		q, refusal := journalQuantity(c.units)
		if refusal != nil {
			t.Fatalf("journalQuantity(%d) refused: %v", c.units, refusal)
		}
		if q.String() != c.want {
			t.Fatalf("journalQuantity(%d) = %s, want %s", c.units, q, c.want)
		}
	}
}

func decimalPtr(s string) *decimal.Decimal {
	d := decimal.RequireFromString(s)
	return &d
}

// TestProjectCurrencyTradeSignsBothLegsBySide is the case the buy fixture
// cannot reach: a SALE of currency. The money goes the other way on both legs —
// rubles in, dollars out — and a rule that signed only the payment would say the
// owner received rubles AND received dollars, which is money from nowhere.
func TestProjectCurrencyTradeSignsBothLegsBySide(t *testing.T) {
	row := mirrorRowFor(t, "currency_buy.json")
	// The same trade seen from the other side: the broker pays 90 000 ₽ for the
	// 1 000 $ it takes back.
	row.Payment = decimal.RequireFromString("90000")
	row.OpType = "OPERATION_TYPE_SELL"

	ops, _, refusal := ProjectRow(row, fixtureAccountID, nil, &TradedCurrency{
		Code: "USD", NominalPerUnit: decimal.RequireFromString("1"),
	})
	if refusal != nil {
		t.Fatalf("a currency sale was refused: %v", refusal)
	}
	byCurrency := map[string]int64{}
	for _, o := range ops {
		if o.Type == operation.TypeConversion {
			byCurrency[o.Currency] = o.AmountMinor
		}
	}
	if byCurrency["RUB"] != 9_000_000 {
		t.Errorf("the ruble leg is %d, want 9000000 — a sale brings rubles IN", byCurrency["RUB"])
	}
	if byCurrency["USD"] != -100_000 {
		t.Errorf("the dollar leg is %d, want -100000 — the dollars LEFT. A positive figure here is money from nowhere: rubles received and dollars received for one exchange", byCurrency["USD"])
	}
}

// TestProjectCurrencyTradeMultipliesByTheNominal is the hundredfold case, and
// the reason the nominal is fetched at all. One unit of the Kyrgyz som
// instrument is a HUNDRED som, so ten units bought is a thousand som — not ten.
// Every currency the owner actually trades has a nominal of one, which is
// exactly why no fixture of his would ever catch this.
func TestProjectCurrencyTradeMultipliesByTheNominal(t *testing.T) {
	row := mirrorRowFor(t, "currency_buy.json")
	row.QuantityDone = 10
	row.Quantity = 10
	// Ten units at 900 ₽ each: the money still divides by the units, which is
	// the check that stands between this rule and a misread quantity.
	row.Payment = decimal.RequireFromString("-9000")
	price := decimal.RequireFromString("900")
	row.Price = &price

	ops, _, refusal := ProjectRow(row, fixtureAccountID, nil, &TradedCurrency{
		Code: "KGS", NominalPerUnit: decimal.RequireFromString("100"),
	})
	if refusal != nil {
		t.Fatalf("the trade was refused: %v", refusal)
	}
	var received int64
	for _, o := range ops {
		if o.Currency == "KGS" {
			received = o.AmountMinor
		}
	}
	switch received {
	case 1_000:
		t.Errorf("the som leg is 1000 — ten units were taken as ten som. One unit is a hundred, so this is wrong by exactly a hundredfold, which is the shape of the most expensive defect this program has had")
	case 100_000:
	default:
		t.Errorf("the som leg is %d, want 100000 (10 units x 100 som, in minor units)", received)
	}
}

// TestProjectCurrencyTradeAllowsTheBrokersOwnRounding is the live case the
// first version of the check got wrong. The broker sends six decimals of price
// and a payment rounded to the kopeck, so the product almost never lands
// exactly: 942 yuan at 12.341497 ₽ is 11 625.690174 ₽ and is paid as
// 11 625.69. An exact comparison refused 44 of the owner's own trades over
// that fraction. The numbers here are one of those rows.
func TestProjectCurrencyTradeAllowsTheBrokersOwnRounding(t *testing.T) {
	row := mirrorRowFor(t, "currency_buy.json")
	row.Quantity, row.QuantityDone = 942, 942
	row.Payment = decimal.RequireFromString("-11625.69")
	price := decimal.RequireFromString("12.341497")
	row.Price = &price

	ops, _, refusal := ProjectRow(row, fixtureAccountID, nil, &TradedCurrency{
		Code: "CNY", NominalPerUnit: decimal.RequireFromString("1"),
	})
	if refusal != nil {
		t.Fatalf("a trade rounded to the kopeck was refused: %v", refusal)
	}
	var received int64
	for _, o := range ops {
		if o.Currency == "CNY" {
			received = o.AmountMinor
		}
	}
	if received != 94_200 {
		t.Errorf("the yuan leg is %d, want 94200 (942 CNY in minor units)", received)
	}
}

// TestProjectCurrencyTradeRefusesMoneyThatDoesNotDivide is the guard against the
// mistake this codebase has already made once with a broker's quantity field:
// `quantity` turned out to be the size of the ORDER, and fifteen trades went
// into the journal at up to two and a half times their real size. If the money
// does not divide by the units at the stated price, this row does not mean what
// the rule assumes, and it becomes visible instead of projected.
func TestProjectCurrencyTradeRefusesMoneyThatDoesNotDivide(t *testing.T) {
	row := mirrorRowFor(t, "currency_buy.json")
	row.QuantityDone = 400 // the payment says 1 000 units at 90 ₽

	_, _, refusal := ProjectRow(row, fixtureAccountID, nil, &TradedCurrency{
		Code: "USD", NominalPerUnit: decimal.RequireFromString("1"),
	})
	if refusal == nil {
		t.Fatalf("a trade whose money does not divide by its units was projected")
	}
	if refusal.Reason != ReasonCurrencyTrade {
		t.Errorf("reason is %q, want %q", refusal.Reason, ReasonCurrencyTrade)
	}
}
