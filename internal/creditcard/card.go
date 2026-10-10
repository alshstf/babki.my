// Package creditcard keeps a credit card's terms and works out where the card
// stands (decision Р-26): the debt and what is left of the limit, what to pay
// by which day so that purchases stay free of interest, the monthly minimum,
// and the purchases whose grace has run out. The journal is the record of
// what was spent and paid; the terms say how the bank counts it.
package creditcard

import (
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/money"
)

// GraceKind is how the interest-free period runs.
type GraceKind string

const (
	// FromStatement: a statement period's purchases are free of interest
	// when paid by the statement's payment date — «до 55 дней» (Т-Банк
	// «Платинум», ВТБ, Альфа after its first month).
	FromStatement GraceKind = "statement"
	// Long: each period's purchases are free for a stretch of days from the
	// period's start — «120 дней» (СберКарта), a year (Альфа's first month).
	Long GraceKind = "long"
)

// Terms are a card's terms. Limit and MinFloor are in minor units of the
// card's currency; rates in percent a year; MinPercent of the debt.
type Terms struct {
	AccountID    uuid.UUID
	Limit        int64
	StatementDay int
	PaymentDays  int
	GraceKind    GraceKind
	GraceDays    int
	MinPercent   decimal.Decimal
	MinFloor     int64
	AnnualRate   decimal.Decimal
	// OwnRate is what the family's own money would earn instead, for
	// weighing the card; nil until named.
	OwnRate *decimal.Decimal
}

var hundred, thousand = decimal.NewFromInt(100), decimal.NewFromInt(1000)

// Validate refuses terms no statement can be worked out from.
func (t Terms) Validate() error {
	rate := func(r decimal.Decimal) bool { return !r.IsNegative() && r.LessThan(thousand) }
	switch {
	case t.Limit < 0:
		return fmt.Errorf("%w: the limit cannot be below zero", family.ErrValidation)
	case t.StatementDay < 1 || t.StatementDay > 31:
		return fmt.Errorf("%w: the statement day is 1 to 31", family.ErrValidation)
	case t.PaymentDays < 0 || t.PaymentDays > 60:
		return fmt.Errorf("%w: the days to pay after a statement are 0 to 60", family.ErrValidation)
	case t.GraceKind != FromStatement && t.GraceKind != Long:
		return fmt.Errorf("%w: the grace runs from the statement or long", family.ErrValidation)
	case t.GraceKind == Long && (t.GraceDays < 1 || t.GraceDays > 1100):
		return fmt.Errorf("%w: a long grace is 1 to 1100 days", family.ErrValidation)
	case t.MinPercent.IsNegative() || t.MinPercent.GreaterThan(hundred):
		return fmt.Errorf("%w: the minimum payment is 0 to 100 percent of the debt", family.ErrValidation)
	case t.MinFloor < 0:
		return fmt.Errorf("%w: the smallest minimum payment cannot be below zero", family.ErrValidation)
	case !rate(t.AnnualRate):
		return fmt.Errorf("%w: the card's rate is from 0 to 999.9999 percent", family.ErrValidation)
	case t.OwnRate != nil && !rate(*t.OwnRate):
		return fmt.Errorf("%w: the own money's rate is from 0 to 999.9999 percent", family.ErrValidation)
	}
	return nil
}

// statementOn is the statement day in the given month: the StatementDay, or
// the month's last day when it is shorter.
func (t Terms) statementOn(year int, month time.Month) time.Time {
	last := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
	return time.Date(year, month, min(t.StatementDay, last), 0, 0, 0, 0, time.UTC)
}

// period is the statement period a day falls in: from the statement on or
// before it up to the next one.
func (t Terms) period(d time.Time) (start, end time.Time) {
	start = t.statementOn(d.Year(), d.Month())
	if start.After(d) {
		prev := d.AddDate(0, 0, -d.Day())
		start = t.statementOn(prev.Year(), prev.Month())
	}
	next := time.Date(start.Year(), start.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	return start, t.statementOn(next.Year(), next.Month())
}

// deadline is the last day a purchase on day d is free of interest.
func (t Terms) deadline(d time.Time) time.Time {
	start, end := t.period(d)
	if t.GraceKind == Long {
		return start.AddDate(0, 0, t.GraceDays-1)
	}
	return end.AddDate(0, 0, t.PaymentDays)
}

// Due is an amount to pay by a day.
type Due struct {
	On     time.Time
	Amount int64
}

// Lost is the part of a period's purchases still owed after its grace ran
// out, and roughly the interest on it so far — the bank counts by its own
// rules; this is the yearly rate from each purchase's day.
type Lost struct {
	From, To time.Time
	Deadline time.Time
	Amount   int64
	Interest int64
}

// Status is where a card stands on a day. Debt is positive when owed; a
// negative one is the family's own money on the card.
type Status struct {
	Debt      int64
	Available int64
	// LastStatement is the latest statement day on or before today and
	// NextStatement the one after it.
	LastStatement, NextStatement time.Time
	// Minimum is what is still to pay of the last statement's minimum
	// payment, by MinimumOn; MinimumMissed when that day has passed. Once it
	// is paid and its day is past, it is the next statement's, worked out
	// from today's debt: MinimumEstimate.
	Minimum         int64
	MinimumOn       time.Time
	MinimumMissed   bool
	MinimumEstimate bool
	// Grace is what to pay by which day so that purchases stay free of
	// interest, soonest first.
	Grace []Due
	Lost  []Lost
	// NonGrace is the cash taken out and the money moved off the card still
	// owed — interest from its first day — with roughly that interest.
	NonGrace         int64
	NonGraceInterest int64
}

// item is one debit still (partly) owed.
type item struct {
	on    time.Time
	left  int64
	grace bool
}

// purchase says whether a journal row is spending the grace covers: a
// withdrawal that is not one half of a move to the family's own account.
// Fees, interest charged and money moved off the card are owed from day one.
func purchase(op operation.Operation) bool {
	return op.Type == operation.TypeWithdrawal && op.TransferGroupID == nil
}

// Work works out the card's status on today from its journal, rows in the
// card's currency only. Payments clear the oldest debt first.
func Work(t Terms, ops []operation.Operation, currency string, today time.Time) Status {
	rows := slices.Clone(ops)
	slices.SortStableFunc(rows, func(a, b operation.Operation) int { return a.OccurredOn.Compare(b.OccurredOn) })
	var items []*item
	var credit int64
	pay := func(amount int64) {
		for _, it := range items {
			if amount == 0 {
				break
			}
			take := min(it.left, amount)
			it.left -= take
			amount -= take
		}
		credit += amount
	}
	for _, op := range rows {
		if op.Currency != currency || op.AmountMinor == 0 || op.OccurredOn.After(today) {
			continue
		}
		if op.AmountMinor > 0 {
			pay(op.AmountMinor)
			continue
		}
		owed := -op.AmountMinor
		take := min(credit, owed)
		credit -= take
		if owed -= take; owed > 0 {
			items = append(items, &item{on: op.OccurredOn, left: owed, grace: purchase(op)})
		}
	}

	var st Status
	for _, it := range items {
		st.Debt += it.left
	}
	st.Debt -= credit
	st.Available = t.Limit - max(st.Debt, 0)
	st.LastStatement, st.NextStatement = t.period(today)

	rate := t.AnnualRate.Div(hundred)
	interest := func(amount int64, from time.Time) int64 {
		days := int64(today.Sub(from).Hours() / 24)
		v, _ := money.Minor(decimal.NewFromInt(amount).Mul(rate).Mul(decimal.NewFromInt(days)).Div(decimal.NewFromInt(365)))
		return v
	}
	grace := map[time.Time]int64{}
	lost := map[time.Time]*Lost{}
	for _, it := range items {
		if it.left == 0 {
			continue
		}
		if !it.grace {
			st.NonGrace += it.left
			st.NonGraceInterest += interest(it.left, it.on)
			continue
		}
		d := t.deadline(it.on)
		if !d.Before(today) {
			grace[d] += it.left
			continue
		}
		from, to := t.period(it.on)
		l, ok := lost[from]
		if !ok {
			l = &Lost{From: from, To: to.AddDate(0, 0, -1), Deadline: d}
			lost[from] = l
		}
		l.Amount += it.left
		l.Interest += interest(it.left, it.on)
	}
	for on, amount := range grace {
		st.Grace = append(st.Grace, Due{On: on, Amount: amount})
	}
	slices.SortFunc(st.Grace, func(a, b Due) int { return a.On.Compare(b.On) })
	for _, l := range lost {
		st.Lost = append(st.Lost, *l)
	}
	slices.SortFunc(st.Lost, func(a, b Lost) int { return a.From.Compare(b.From) })

	// The minimum of the last statement: a share of the debt it showed, not
	// less than the floor nor more than the debt, less what was paid since.
	var atStatement, paidSince int64
	for _, op := range rows {
		if op.Currency != currency || op.OccurredOn.After(today) {
			continue
		}
		if op.OccurredOn.Before(st.LastStatement) {
			atStatement -= op.AmountMinor
		} else if op.AmountMinor > 0 {
			paidSince += op.AmountMinor
		}
	}
	st.MinimumOn = st.LastStatement.AddDate(0, 0, t.PaymentDays)
	if minimum := t.minimum(atStatement); minimum > paidSince {
		st.Minimum = minimum - paidSince
		st.MinimumMissed = st.MinimumOn.Before(today)
	}
	if st.Minimum == 0 && st.MinimumOn.Before(today) {
		st.nextMinimum(t)
	}
	return st
}

// nextMinimum is the next statement's minimum, from today's debt.
func (st *Status) nextMinimum(t Terms) {
	st.MinimumOn = st.NextStatement.AddDate(0, 0, t.PaymentDays)
	st.Minimum = t.minimum(st.Debt)
	st.MinimumEstimate = true
}

// minimum is the minimum payment on a debt: MinPercent of it, not less than
// MinFloor, never more than the debt itself.
func (t Terms) minimum(debt int64) int64 {
	if debt <= 0 {
		return 0
	}
	share, _ := money.Minor(decimal.NewFromInt(debt).Mul(t.MinPercent).Div(hundred))
	return min(max(share, t.MinFloor), debt)
}

// ByBalance is the status of a card known only by its balance: the debt, the
// limit left and the minimum that debt calls for at the next statement. What
// keeps the grace cannot be told without the purchases.
func ByBalance(t Terms, balance int64, today time.Time) Status {
	st := Status{Debt: -balance}
	st.Available = t.Limit - max(st.Debt, 0)
	st.LastStatement, st.NextStatement = t.period(today)
	// The balance may already hold payments made since the last statement, so
	// its minimum is an estimate either way: the last statement's while its
	// day is ahead, else the next one's.
	st.MinimumOn = st.LastStatement.AddDate(0, 0, t.PaymentDays)
	st.Minimum = t.minimum(st.Debt)
	st.MinimumEstimate = true
	if st.MinimumOn.Before(today) {
		st.nextMinimum(t)
	}
	return st
}

// Benefit weighs the card against the family's own money over a stretch: what
// the own money earned meanwhile — at OwnRate, on the debt the card carried
// each day, the money not taken from savings — plus the cashback, less the
// interest and fees the bank charged, and less Pending: roughly the interest
// the lost grace and the money moved off the card run up, not in the journal
// yet. Without OwnRate the first part is nought and OwnRateKnown false.
type Benefit struct {
	From, To     time.Time
	OwnRateKnown bool
	OwnEarned    int64
	Cashback     int64
	Costs        int64
	Pending      int64
	Total        int64
}

// Kinds sorts the card's rows for Weigh: categories that mean cashback, and
// categories that mean a charge (interest on credit, bank fees).
type Kinds struct {
	Cashback map[uuid.UUID]bool
	Charges  map[uuid.UUID]bool
}

func (k Kinds) cashback(op operation.Operation) bool {
	return op.AmountMinor > 0 && (op.Type == operation.TypeInterest || op.CategoryID != nil && k.Cashback[*op.CategoryID])
}

func (k Kinds) charge(op operation.Operation) bool {
	if op.AmountMinor >= 0 {
		return false
	}
	switch op.Type {
	case operation.TypeFee, operation.TypeTax, operation.TypeInterest:
		return true
	}
	return op.CategoryID != nil && k.Charges[*op.CategoryID]
}

// Weigh is the card's benefit over the year to today, or since its first row
// when that is later.
func Weigh(t Terms, ops []operation.Operation, currency string, today time.Time, kinds Kinds) Benefit {
	rows := make([]operation.Operation, 0, len(ops))
	for _, op := range ops {
		if op.Currency == currency && op.AmountMinor != 0 && !op.OccurredOn.After(today) {
			rows = append(rows, op)
		}
	}
	slices.SortStableFunc(rows, func(a, b operation.Operation) int { return a.OccurredOn.Compare(b.OccurredOn) })
	b := Benefit{From: today.AddDate(-1, 0, 0), To: today, OwnRateKnown: t.OwnRate != nil}
	if len(rows) == 0 {
		b.From = today
		return b
	}
	if rows[0].OccurredOn.After(b.From) {
		b.From = rows[0].OccurredOn
	}
	var debt int64
	k := 0
	for ; k < len(rows) && rows[k].OccurredOn.Before(b.From); k++ {
		debt -= rows[k].AmountMinor
	}
	var debtDays int64
	for day := b.From; !day.After(today); day = day.AddDate(0, 0, 1) {
		for ; k < len(rows) && !rows[k].OccurredOn.After(day); k++ {
			op := rows[k]
			debt -= op.AmountMinor
			switch {
			case kinds.cashback(op):
				b.Cashback += op.AmountMinor
			case kinds.charge(op):
				b.Costs -= op.AmountMinor
			}
		}
		debtDays += max(debt, 0)
	}
	if t.OwnRate != nil {
		b.OwnEarned, _ = money.Minor(decimal.NewFromInt(debtDays).Mul(*t.OwnRate).Div(decimal.NewFromInt(36500)))
	}
	b.Total = b.OwnEarned + b.Cashback - b.Costs
	return b
}
