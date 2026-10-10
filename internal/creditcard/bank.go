package creditcard

import (
	"fmt"
	"time"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
)

// BankFigures are what the bank itself says is due (decision Р-32): the
// payment that keeps the grace and its day, the minimum and its day, as the
// person read them from the bank's app or statement on StatedOn. Until their
// day passes they stand over the card's own reckoning — in what the card
// shows and in the reminders — and the two are set side by side: a gap
// between them is a rule of the bank's the terms do not tell.
type BankFigures struct {
	StatedOn time.Time
	Grace    *Due
	Minimum  *Due
}

// Validate refuses figures that cannot be what a bank says.
func (b BankFigures) Validate(today time.Time) error {
	if b.StatedOn.IsZero() || b.StatedOn.After(today) {
		return fmt.Errorf("%w: the bank's figures are read on a day, not a later one", family.ErrValidation)
	}
	for _, d := range []*Due{b.Grace, b.Minimum} {
		if d != nil && (d.Amount < 0 || d.On.IsZero()) {
			return fmt.Errorf("%w: a sum the bank says is due is not below zero and has its day", family.ErrValidation)
		}
	}
	if b.Grace == nil && b.Minimum == nil {
		return fmt.Errorf("%w: say at least the minimum or what keeps the grace", family.ErrValidation)
	}
	return nil
}

// BankView is the bank's figures against today.
type BankView struct {
	StatedOn time.Time
	Grace    *BankDue
	Minimum  *BankDue
}

// BankDue is one of the bank's figures: what it said, what is left of it
// after the card's payments since, and what the card's own reckoning says
// for the same (nil when it says nothing) — Agrees when the day is the same
// and the sums within a rouble or a percent.
type BankDue struct {
	On     time.Time
	Amount int64
	Left   int64
	Ours   *Due
	Agrees bool
}

// WithBank sets the bank's figures still ahead against the card's
// reckoning, less the payments in the card's currency since they were read
// (none known for a card counted by its balance). A figure whose day has
// passed is history: the card counts by its own reckoning again until the
// next is read.
func (st *Status) WithBank(b *BankFigures, ops []operation.Operation, currency string, today time.Time) {
	if b == nil {
		return
	}
	var paid int64
	for _, op := range ops {
		if op.Currency == currency && op.AmountMinor > 0 && op.OccurredOn.After(b.StatedOn) && !op.OccurredOn.After(today) {
			paid += op.AmountMinor
		}
	}
	view := func(d *Due, ours *Due) *BankDue {
		if d == nil || d.On.Before(today) {
			return nil
		}
		v := &BankDue{On: d.On, Amount: d.Amount, Left: max(d.Amount-paid, 0), Ours: ours}
		if ours != nil {
			gap := ours.Amount - v.Left
			v.Agrees = ours.On.Equal(d.On) && (abs(gap) <= 1_00 || abs(gap)*100 <= v.Left)
		}
		return v
	}
	var ourGrace, ourMinimum *Due
	if len(st.Grace) > 0 {
		ourGrace = &st.Grace[0]
	}
	if st.Minimum > 0 || !st.MinimumOn.IsZero() {
		ourMinimum = &Due{On: st.MinimumOn, Amount: st.Minimum}
	}
	grace, minimum := view(b.Grace, ourGrace), view(b.Minimum, ourMinimum)
	if grace == nil && minimum == nil {
		return
	}
	st.Bank = &BankView{StatedOn: b.StatedOn, Grace: grace, Minimum: minimum}
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
