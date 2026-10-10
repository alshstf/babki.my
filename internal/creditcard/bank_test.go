package creditcard

import (
	"testing"

	"babki.my/babki/internal/operation"
)

// What the bank says (decision Р-32) stands over the card's reckoning until
// its day: what is left of it after the payments since, and whether the
// reckoning agrees.
func TestTheBanksFiguresStandOverTheReckoning(t *testing.T) {
	ops := []operation.Operation{spend("2026-09-03", 30_000), spend("2026-09-20", 22_300)}
	st := Work(alfa, ops, "RUB", d("2026-10-07"), Kinds{})
	// The bank: 52 300 by the 21st to keep the grace — as reckoned — and a
	// minimum of 1 600 by the 21st, where we reckon 1 569.
	b := &BankFigures{
		StatedOn: d("2026-10-02"),
		Grace:    &Due{On: d("2026-10-21"), Amount: 52_300_00},
		Minimum:  &Due{On: d("2026-10-21"), Amount: 1_600_00},
	}
	st.WithBank(b, ops, "RUB", d("2026-10-07"))
	if st.Bank == nil || st.Bank.Grace == nil || !st.Bank.Grace.Agrees || st.Bank.Grace.Left != 52_300_00 {
		t.Fatalf("grace = %+v", st.Bank)
	}
	if st.Bank.Minimum == nil || st.Bank.Minimum.Agrees || st.Bank.Minimum.Ours.Amount != 1_569_00 {
		t.Errorf("minimum = %+v, want a gap with our 1 569", st.Bank.Minimum)
	}

	// Paid 10 000 on the 8th: 42 300 left of the bank's figure, and of ours.
	paid := append(append([]operation.Operation{}, ops...), repay("2026-10-08", 10_000))
	st = Work(alfa, paid, "RUB", d("2026-10-09"), Kinds{})
	st.WithBank(b, paid, "RUB", d("2026-10-09"))
	if st.Bank.Grace.Left != 42_300_00 || !st.Bank.Grace.Agrees || st.Bank.Minimum.Left != 0 {
		t.Errorf("after paying: %+v %+v", st.Bank.Grace, st.Bank.Minimum)
	}

	// Past its day, it is history.
	st = Work(alfa, paid, "RUB", d("2026-10-22"), Kinds{})
	st.WithBank(b, paid, "RUB", d("2026-10-22"))
	if st.Bank != nil {
		t.Errorf("past its day: %+v", st.Bank)
	}
	if (BankFigures{StatedOn: d("2026-10-02")}).Validate(d("2026-10-07")) == nil {
		t.Error("figures with nothing said were accepted")
	}
	if b.Validate(d("2026-10-01")) == nil {
		t.Error("figures read on a later day were accepted")
	}
}
