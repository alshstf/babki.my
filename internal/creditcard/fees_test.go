package creditcard

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/operation"
)

// moved is money moved off the card on a day: to the family's cash when
// cash says so, otherwise a transfer.
func moved(on string, rub int64, cash map[uuid.UUID]bool, isCash bool) operation.Operation {
	op := spend(on, rub)
	op.ID = uuid.New()
	group := uuid.New()
	op.TransferGroupID = &group
	if isCash {
		cash[op.ID] = true
	}
	return op
}

// Т-Банк «Платинум»: transfers through the bank's services free up to
// 80 000 a period, past it 4.9% and 490 (#462).
func TestTransfersAreFreeUpToThePeriodsPart(t *testing.T) {
	fees := Fees{TransferFree: 80_000_00, TransferPercent: decimal.RequireFromString("4.9"), TransferFixed: 490_00}
	if got := fees.Transfer(30_000_00, 40_000_00, 0); got != 0 {
		t.Errorf("a transfer within the free part costs %d", got)
	}
	if got := fees.Transfer(30_000_00, 60_000_00, 0); got != 490_00+490_00 {
		t.Errorf("a transfer 10 000 past the free part costs %d, want 490 + 490", got)
	}

	card := alfa
	card.Fees = fees
	cash := map[uuid.UUID]bool{}
	ops := []operation.Operation{
		moved("2026-08-20", 5_000, cash, false), // last period's
		moved("2026-09-03", 20_000, cash, false),
		moved("2026-09-05", 7_000, cash, true),
		moved("2026-09-08", 15_000, cash, false),
	}
	st := Work(card, ops, "RUB", d("2026-09-10"), Kinds{Cash: cash})
	if st.TransfersThisPeriod != 35_000_00 || st.CashThisPeriod != 7_000_00 {
		t.Errorf("transfers %d, cash %d this period; want 35 000 and 7 000", st.TransfersThisPeriod, st.CashThisPeriod)
	}
}

// ВТБ «Карта возможностей»: cash and transfers free up to 50 000 in all in
// the first 30 days, past it 5.9% and 590 (#462).
func TestTheFirstDaysCashAndTransfersAreFree(t *testing.T) {
	opened := d("2026-09-01")
	card := alfa
	card.OpenedOn = &opened
	card.Fees = Fees{
		IntroDays: 30, IntroFree: 50_000_00,
		CashPercent: decimal.RequireFromString("5.9"), CashFixed: 590_00,
		TransferPercent: decimal.RequireFromString("5.9"), TransferFixed: 590_00,
	}
	if err := card.Validate(); err != nil {
		t.Fatal(err)
	}
	cash := map[uuid.UUID]bool{}
	ops := []operation.Operation{moved("2026-09-05", 20_000, cash, true), moved("2026-09-10", 10_000, cash, false)}

	st := Work(card, ops, "RUB", d("2026-09-15"), Kinds{Cash: cash})
	if st.IntroLeft != 20_000_00 || !st.IntroUntil.Equal(d("2026-09-30")) {
		t.Errorf("first days: %d left until %s, want 20 000 until the 30th", st.IntroLeft, st.IntroUntil.Format(time.DateOnly))
	}
	if got := card.Fees.Cash(30_000_00, st.CashThisPeriod, st.IntroLeft); got != 590_00+590_00 {
		t.Errorf("cash 10 000 past the first days' part costs %d, want 590 + 590", got)
	}
	if got := card.Fees.Transfer(20_000_00, st.TransfersThisPeriod, st.IntroLeft); got != 0 {
		t.Errorf("a transfer within the first days' part costs %d", got)
	}

	st = Work(card, ops, "RUB", d("2026-10-01"), Kinds{Cash: cash})
	if st.IntroLeft != 0 || !st.IntroUntil.IsZero() {
		t.Errorf("after the first days: %d left until %s", st.IntroLeft, st.IntroUntil)
	}

	card.OpenedOn = nil
	if card.Validate() == nil {
		t.Error("the first days without the contract's day are taken")
	}
	card.OpenedOn, card.Fees.IntroFree = &opened, 0
	if card.Validate() == nil {
		t.Error("the first days without an amount are taken")
	}
}

// The penalty in percent a year (Т-Банк: 20%) and from the 6th day of
// lateness («Халва»), on the Газпромбанк case of TestTheTariffsFeesAreToldAhead:
// August's minimum of 500 missed by the 31st, ten days late on September 10.
func TestThePenaltyRunsAYearsRateOrFromItsDay(t *testing.T) {
	late := []operation.Operation{spend("2026-07-03", 10_000)}
	card := gpb180()
	card.Fees = Fees{PenaltyYearly: decimal.NewFromInt(20)}
	// 500 × 10 days × 20% / 365.
	if st := Work(card, late, "RUB", d("2026-09-10"), Kinds{}); st.Penalty != 2_74 {
		t.Errorf("penalty at 20%% a year = %d, want 2.74", st.Penalty)
	}

	card.Fees = Fees{PenaltyDaily: decimal.RequireFromString("0.1"), PenaltyFromDay: 6}
	// Days 6 to 10: 500 × 5 × 0.1%.
	if st := Work(card, late, "RUB", d("2026-09-10"), Kinds{}); st.Penalty != 2_50 {
		t.Errorf("penalty from the 6th day = %d, want 2.50", st.Penalty)
	}
	if st := Work(card, late, "RUB", d("2026-09-05"), Kinds{}); st.Penalty != 0 {
		t.Errorf("penalty on the 5th day = %d, want none yet", st.Penalty)
	}

	card.Fees = Fees{PenaltyDaily: decimal.RequireFromString("0.1"), PenaltyYearly: decimal.NewFromInt(20)}
	if card.Validate() == nil {
		t.Error("a penalty both a day and a year is taken")
	}
}

// Т-Банк: the yearly fee comes with the statement after the first spending
// and every twelfth after it, into that statement's minimum (#462).
func TestTheYearlyFeeComesWithItsStatement(t *testing.T) {
	card := alfa
	card.StatementDay = 10
	card.Fees = Fees{Yearly: 590_00}
	ops := []operation.Operation{spend("2026-03-05", 10_000)}

	st := Work(card, ops, "RUB", d("2026-03-07"), Kinds{})
	if !st.YearlyFeeOn.Equal(d("2026-03-10")) {
		t.Errorf("the first yearly fee comes on %s, want the 10th of March", st.YearlyFeeOn.Format(time.DateOnly))
	}
	// After the March statement, the next is a year on; its minimum has it.
	st = Work(card, ops, "RUB", d("2026-03-15"), Kinds{})
	if !st.YearlyFeeOn.Equal(d("2027-03-10")) {
		t.Errorf("after March the yearly fee comes on %s, want 2027-03-10", st.YearlyFeeOn.Format(time.DateOnly))
	}
	if want := card.minimum(10_590_00, 0); st.Minimum != want {
		t.Errorf("March's minimum = %d, want %d with the fee", st.Minimum, want)
	}
	// In a month without it, the minimum has none.
	ops = append(ops, repay("2026-04-01", 10_000), spend("2026-04-12", 10_000))
	st = Work(card, ops, "RUB", d("2026-05-15"), Kinds{})
	if want := card.minimum(10_000_00, 0); st.Minimum != want {
		t.Errorf("May's minimum = %d, want %d without the fee", st.Minimum, want)
	}
	// A charge of the bank's in the journal is the fee, not counted again: on
	// the statement's own day it is the next statement's, as every row of
	// that day.
	fee := operation.Operation{Type: operation.TypeFee, OccurredOn: d("2026-03-10"), AmountMinor: -590_00, Currency: "RUB"}
	st = Work(card, []operation.Operation{spend("2026-03-05", 10_000), fee}, "RUB", d("2026-03-15"), Kinds{})
	if want := card.minimum(10_000_00, 0); st.Minimum != want {
		t.Errorf("with the fee in the journal the minimum = %d, want %d", st.Minimum, want)
	}
}
