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

// Package tinvest, to reach mskDay, journalQuantity and the wire decoder.
// Every expected value is a literal, never computed from the implementation's
// constants, so a changed rule cannot drag its expectation along.

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

// loadOperationItem reads a fixture as the client reads the wire, through
// wireOperationItem, so fixtures keep the gateway's shape (int64 as strings).
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

// mirrorRowFor builds the mirror row the store would write, through the insert's
// own helpers (upperCurrency, moneyOrNothing, contentKey).
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
		ClassCode:          it.ClassCode,
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

// #131 on a real broker row: 115 sold of 190 ordered. The payment matches
// the first, so the journal takes it; both are literals.
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
	// The money is the same either way; a wrong count corrupts the price per
	// unit, so the two are checked together.
	if op.AmountMinor != 12712100 {
		t.Errorf("amount_minor = %d, want 12712100", op.AmountMinor)
	}
	if op.FeeMinor != 4770 {
		t.Errorf("fee_minor = %d, want 4770", op.FeeMinor)
	}
}

// A trade with no executed quantity is refused, not measured by its order.
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

// A commission in another currency becomes its own entry with no
// instrument: one row holds one currency, and an instrument would make the
// account unreadable (see TestProjectedOperationsFoldThroughTheEngine).
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

// Without the traded currency a currency purchase stays unparsed, even with a
// resolved instrument passed. The code must be currency_trade specifically: with
// the branch removed, instrumentRefusal would still refuse with
// unsupported_type, a different statement.
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
		// Interest names a share here and still gets no instrument: the engine
		// refuses interest with one.
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

// A tax correction arrives positive (seven of nine on the owner's account)
// and is recorded as a tax with that sign: a tax folds into income by its signed
// amount, so a refund restores what the withholding took. Hand entry still
// refuses a positive tax (see operation.validateImported).
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

// An ordinary tax stays a tax.
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

// A zero tax is not a refund; it goes to the journal, whose own refusal the
// owner reads.
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

// An amortization records no quantity: no units move.
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

// A redemption's commission goes where a trade's does: FeeMinor in the row's
// currency, its own entry otherwise.
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

// The live shape: a full redemption reported as money only. The row is read;
// the sale is built without a quantity and deferred to the rebuild
// (closeRedemptions). The commission stays on the same entry.
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

// A redemption with a count is complete; without this, deferring every
// redemption would pass above.
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

// Decision 5: one entry would lose the income or invent cash.
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

// Decision 9: shares from another broker have basis zero and say why, on
// that case only.
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
	if op.Note != "Перевод бумаг от другого брокера — стоимость приобретения брокер не передаёт" {
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

// A move between own accounts reads its direction from the quantity's sign
// (see projectSecuritiesTransfer).
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

// A zero move between own accounts is refused for its missing direction.
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

// The owner's two rows: 0.24 of a Warner Bros. Discovery share arrived as 0 in
// every field, 44 380,35 fund units as 44 380; the real number is only in the
// description. The fraction is taken because its whole part equals the field.
func TestProjectRowTransferReadsAFractionFromTheDescriptionWithProof(t *testing.T) {
	t.Run("a part of a share the field reports as nought", func(t *testing.T) {
		row := mirrorRowFor(t, "input_securities.json")
		row.Quantity = 0
		row.Description = "Завод 0.24 акций Warner Bros. Discovery из другого депозитария"
		op := projectOne(t, row, resolvedShare())
		if op.Type != operation.TypeTransferIn {
			t.Errorf("type = %s, want transfer_in", op.Type)
		}
		if op.Quantity == nil || op.Quantity.String() != "0.24" {
			t.Errorf("quantity = %v, want 0.24 — the description's figure, whose whole part (0) is the field's", op.Quantity)
		}
	})

	// 0.75 against a field of 0: the proof is the whole part, not the nearest
	// integer; rounding would give 1 and refuse an agreeing pair. Other fixtures
	// are below a half, where the two agree.
	t.Run("a part of a share past the half the field still reports as nought", func(t *testing.T) {
		row := mirrorRowFor(t, "input_securities.json")
		row.Quantity = 0
		row.Description = "Завод 0.75 акций Warner Bros. Discovery из другого депозитария"
		op := projectOne(t, row, resolvedShare())
		if op.Quantity == nil || op.Quantity.String() != "0.75" {
			t.Errorf("quantity = %v, want 0.75 — the whole part of 0.75 is 0, which is what the field says", op.Quantity)
		}
	})

	t.Run("fund units the field reports by their whole part", func(t *testing.T) {
		row := mirrorRowFor(t, "output_securities.json")
		row.Quantity = 44380
		row.Description = "Вывод 44380.35 лотов фонда Технологии Америки в другой депозитарий"
		op := projectOne(t, row, resolvedShare())
		if op.Type != operation.TypeTransferOut {
			t.Errorf("type = %s, want transfer_out", op.Type)
		}
		if op.Quantity == nil || op.Quantity.String() != "44380.35" {
			t.Errorf("quantity = %v, want 44380.35 — the description's figure, whose whole part (44380) is the field's", op.Quantity)
		}
	})
}

// A fraction whose whole part is not the field's number: refused as a
// contradiction.
func TestProjectRowTransferWhoseDescriptionContradictsTheFieldIsRefused(t *testing.T) {
	row := mirrorRowFor(t, "output_securities.json")
	row.Quantity = 44381
	row.Description = "Вывод 44380.35 лотов фонда Технологии Америки в другой депозитарий"
	ops, _, refusal := ProjectRow(row, fixtureAccountID, resolvedShare(), nil)

	if len(ops) != 0 {
		t.Fatalf("got %d operations, want none — the field says 44381 and the description 44380.35, and a row built from either is this program picking which half of the broker's message to believe", len(ops))
	}
	if refusal == nil {
		t.Fatal("a transfer whose field and description disagree was projected instead of being refused")
	}
	if refusal.Reason != ReasonTransferQuantityContradicted {
		t.Errorf("reason = %q, want transfer_quantity_contradicted", refusal.Reason)
	}
	// Both numbers travel into the detail: a reader deciding which is right
	// needs to see the pair.
	if !strings.Contains(refusal.Detail, "44381") || !strings.Contains(refusal.Detail, "44380.35") {
		t.Errorf("detail = %q, want both the field's 44381 and the description's 44380.35 in it", refusal.Detail)
	}
}

// A zero field and no readable fraction: a one-sided transfer refused for
// the missing number (the two-sided kind refuses earlier for direction). A comma
// is not read: the broker has never been seen to write one.
func TestProjectRowOneSidedTransferOfNothingNamesTheMissingNumber(t *testing.T) {
	for _, description := range []string{
		"Завод акций Warner Bros. Discovery из другого депозитария",
		"Завод 0,24 акций Warner Bros. Discovery из другого депозитария",
	} {
		t.Run(description, func(t *testing.T) {
			row := mirrorRowFor(t, "input_securities.json")
			row.Quantity = 0
			row.Description = description
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
			// The description goes into the detail for whoever retypes the row.
			if !strings.Contains(refusal.Detail, description) {
				t.Errorf("detail = %q, want the broker's own description in it", refusal.Detail)
			}
		})
	}
}

// A whole number in the prose is not compared; the field stands.
func TestProjectRowTransferWithAWholeFigureKeepsTheField(t *testing.T) {
	for _, tc := range []struct {
		field int64
		want  string
	}{
		{2400, "2400"},
		{2401, "2401"},
	} {
		row := mirrorRowFor(t, "input_securities.json")
		row.Quantity = tc.field
		row.Description = "Завод 2400 лотов фонда FinEx Акции американских компаний из другого депозитария"
		op := projectOne(t, row, resolvedShare())
		if op.Quantity == nil || op.Quantity.String() != tc.want {
			t.Errorf("field %d: quantity = %v, want %s — the field, whatever whole number the prose restates", tc.field, op.Quantity, tc.want)
		}
	}
}

// The owner's case (2026-08-22): Т-Капитал redeemed 73 % of a fund and the
// broker sent BOND_REPAYMENT_FULL; the bond rule closed the 27 % still held. The
// catalog type decides; bond fixtures are reused because the row looks the
// same.
func TestProjectRowPayoutOnAFundIsRefusedNotBookedAsARedemption(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture string
		typ     instrument.Type
		// The fixture's payment written out, not read back from the field the
		// code copies.
		payment string
	}{
		{"a full redemption of an etf without a count", "bond_repayment_full_no_quantity.json", instrument.TypeETF, "10000"},
		{"a full redemption of an etf with a count", "bond_repayment_full.json", instrument.TypeETF, "10000"},
		{"a full redemption of a share", "bond_repayment_full_no_quantity.json", instrument.TypeShare, "10000"},
		{"a partial repayment of an etf", "bond_repayment.json", instrument.TypeETF, "200"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := mirrorRowFor(t, tc.fixture)
			ops, deferred, refusal := ProjectRow(row, fixtureAccountID, &Resolved{InstrumentID: fixtureInstrID, Type: tc.typ}, nil)
			if len(ops) != 0 {
				t.Fatalf("got %d operations, want none — a fund's payout booked as a bond's redemption closes units nobody redeemed", len(ops))
			}
			if deferred != DeferredNothing {
				t.Errorf("deferred = %d, want DeferredNothing (%d): a refused row must not leave the rebuild a count to fill in", deferred, DeferredNothing)
			}
			if refusal == nil {
				t.Fatal("a payout on a fund was projected instead of being refused")
			}
			if refusal.Reason != ReasonFundPayoutUnitsUnknown {
				t.Errorf("reason = %q, want fund_payout_units_unknown", refusal.Reason)
			}
			// The money is in the detail: the row is refused for the units,
			// not for the sum, and the sum is what the owner retypes by hand.
			if !strings.Contains(refusal.Detail, tc.payment) {
				t.Errorf("detail = %q, want the payment %s in it", refusal.Detail, tc.payment)
			}
		})
	}

	t.Run("a bond is unchanged", func(t *testing.T) {
		row := mirrorRowFor(t, "bond_repayment_full_no_quantity.json")
		_, deferred, refusal := ProjectRow(row, fixtureAccountID, &Resolved{InstrumentID: fixtureInstrID, Type: instrument.TypeBond}, nil)
		if refusal != nil {
			t.Fatalf("refused: %v — the bond rule is the bond rule", refusal)
		}
		if deferred != DeferredRedeemedQuantity {
			t.Errorf("deferred = %d, want DeferredRedeemedQuantity (%d)", deferred, DeferredRedeemedQuantity)
		}
	})
}

// A transfer kind belongs to a transfer shape and to no other: a move without
// one would file a departure as an arrival; a kind elsewhere would be read by
// nothing.
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
	// In, out, and the two own-account moves.
	if moves != 4 {
		t.Errorf("%d securities-move types in the table, want 4", moves)
	}
}

// A shape added to the table with no branch must refuse rather than project to
// nothing; reachable only through a change to the code, so the test adds one.
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

// A broker fee charged as its own operation is built and deferred, not dropped:
// one of the owner's 311 is the only record of its charge (see
// DeferredBrokerFeeVerdict).
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
	// A futures delivery fixture plus four typed types: repo taxes,
	// expirations, "unspecified", and an invented one.
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

// Cancelled, in-progress and withdrawn operations produce nothing and no
// refusal.
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

// A commission finer than a kopeck refuses the trade.
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

// A positive commission (a refund) is refused rather than booked as a charge;
// zero is ordinary.
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

// The Moscow day rule at its boundaries (21:00Z and the second before), with
// typed-out days.
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

// acceptsInstrument asks portfolio.Compute type by type.
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
	// Every journal type, counted and validated, so a new one is not
	// skipped.
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

// A purchase, its sale, a dividend and a foreign trade with a local
// commission fold through the engine into the broker's figures. It fails if the
// commission leg carries an instrument.
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

// journalQuantity's quantization. The refusal is unreachable with an int64
// input, and this test does not pretend otherwise (see journalQuantity). The
// extremes are typed out.
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

// A currency sale: both legs flip, or the owner would receive roubles and
// dollars at once.
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

// The nominal case: ten units of the Kyrgyz som instrument are a thousand som,
// not ten. The owner's currencies all have nominal one, so his data would never
// catch this.
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

// Live rounding: 942 yuan at 12.341497 ₽ is 11 625.690174, paid 11 625.69;
// an exact check refused 44 of the owner's trades.
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

// Money that does not divide by units at the price is refused, as the
// order-size misreading would be.
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
