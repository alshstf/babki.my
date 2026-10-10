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
	// Windows: the purchases of WindowMonths periods in a row, counted from
	// the period the card's contract was made in, are free until the end of
	// the GraceMonths-th period from the window's start — «180 дней»
	// Газпромбанка: two months of purchases, paid by the end of the sixth
	// (decision Р-28).
	Windows GraceKind = "windows"
	// Running: one grace for every purchase while the card is in debt — it
	// starts with the first purchase (RunFrom says how), lasts GraceDays,
	// and a new one starts only once the debt is repaid in full; a deadline
	// missed takes it off the whole debt until then (ВТБ «110 дней», Альфа).
	Running GraceKind = "running"
)

// RunFrom is the day a Running grace counts from.
type RunFrom string

const (
	// FromPurchase: the day of the first purchase.
	FromPurchase RunFrom = "purchase"
	// FromNextDay: the day after it (Альфа).
	FromNextDay RunFrom = "next_day"
	// FromMonthStart: the 1st of its month (ВТБ).
	FromMonthStart RunFrom = "month_start"
)

// runStart is the first day of a Running grace opened by a purchase on d.
func (r RunFrom) runStart(d time.Time) time.Time {
	switch r {
	case FromNextDay:
		return d.AddDate(0, 0, 1)
	case FromMonthStart:
		return time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return d
}

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
	// WindowMonths, GraceMonths and OpenedOn are a Windows grace's: the
	// periods of purchases in a window, the periods from its start to its
	// deadline, and the day the contract was made, whose period the windows
	// count from.
	WindowMonths int
	GraceMonths  int
	OpenedOn     *time.Time
	// RunFrom is a Running grace's start; GraceDays its length.
	RunFrom RunFrom
	// GraceAllLost: a deadline missed takes the grace off the whole debt,
	// the later periods' purchases too, and the purchases made after it are
	// charged from their day — until the purchases are repaid in full; a
	// minimum missed does the same until the whole debt is.
	GraceAllLost bool
	// MissedMinimumPeriod: a minimum missed takes the grace off the
	// purchases of the period it was due in — those its next statement
	// shows (Т-Банк) — and no others.
	MissedMinimumPeriod bool
	// PayByPeriodEnd: the minimum is due by the last day of the period after
	// the statement, not PaymentDays after it.
	PayByPeriodEnd bool
	// PayDay: the minimum is due by this day of the month, the first one
	// after the statement (ВТБ: the 20th); 0 for none.
	PayDay int
	// MinRoundUp: the minimum is rounded up to a multiple of it (ВТБ,
	// Т-Банк: 100 ₽), never past the debt; 0 for none.
	MinRoundUp int64
	// ChargesInFull: the minimum is MinPercent of the debt for purchases,
	// cash and transfers (not less than MinFloor) plus the interest and fees
	// charged, in full.
	ChargesInFull bool
	// TransferCategories are the spending categories the bank takes for
	// transfers, not purchases — no grace, interest from the day (decision
	// Р-29); their subcategories with them.
	TransferCategories []uuid.UUID
	// GraceMoves: the cash taken out and the money moved off the card have
	// the grace too, as purchases do (Альфа's «без % на всё»).
	GraceMoves bool
	// GracePeriods: a FromStatement grace's purchases are paid by the
	// payment day of the statement so many periods after the one closing
	// their period (Ozon: 1, «до 80 дней»); GraceCategories give a
	// category's purchases their own number, its subcategories with it
	// (Ozon's «до 140 дней»: 3).
	GracePeriods    int
	GraceCategories []CategoryPeriods
	// GraceToMonthEnd: a long or running grace's last day moves to the last
	// day of its month (Альфа, contracts from 10.08.2026).
	GraceToMonthEnd bool
	Fees            Fees
	Cashback        Cashback
	// Installment: every purchase of the card in installments when Months
	// is above 0 — a card of installments («Халва», decision Р-33).
	Installment Plan
	// Catalog is the catalog's version the terms were taken from; nil when
	// they were stated by hand.
	Catalog *CatalogRef
}

// Plan is a purchase in installments: Months equal parts, one with each
// statement after it, the last taking what rounding left; MonthlyFeePercent
// of its sum a month on top (ВТБ), and Fee once with the first part
// («Халва»).
type Plan struct {
	Months            int
	MonthlyFeePercent decimal.Decimal
	Fee               int64
}

// Validate refuses a plan no parts can be worked out from.
func (p Plan) Validate() error {
	switch {
	case p.Months < 0 || p.Months > 60:
		return fmt.Errorf("%w: installments are 1 to 60 months", family.ErrValidation)
	case p.MonthlyFeePercent.IsNegative() || p.MonthlyFeePercent.GreaterThanOrEqual(hundred) || p.Fee < 0:
		return fmt.Errorf("%w: an installment's fee is 0 to 99.999 percent a month, and not below zero", family.ErrValidation)
	}
	return nil
}

// Installment is where a purchase in installments stands: the parts the
// statements have shown, what of its sum is still to be shown, and the next
// part with its fee.
type Installment struct {
	OperationID uuid.UUID
	On          time.Time
	Amount      int64
	Plan        Plan
	Billed      int
	Left        int64
	Next        int64
	Note        string
}

// part is the installment's next part and its fee.
func (in *Installment) part() (principal, fee int64) {
	months := int64(in.Plan.Months)
	principal = in.Amount / months
	if in.Billed == in.Plan.Months-1 {
		principal = in.Amount - principal*(months-1)
	}
	fee = percentOf(in.Amount, in.Plan.MonthlyFeePercent)
	if in.Billed == 0 {
		fee += in.Plan.Fee
	}
	return principal, fee
}

// Cashback is the card's cashback rules (decision Р-31), to tell it before
// it comes: BasePercent of every purchase, or a category's own percent where
// it is higher (its subcategories with it); not more than MonthlyCap a
// period when one is named; as the bank's points rather than money (Points);
// coming CreditDays after the period's statement.
type Cashback struct {
	BasePercent decimal.Decimal
	Categories  []CategoryPercent
	MonthlyCap  int64
	Points      bool
	CreditDays  int
}

// CategoryPeriods is a category's own GracePeriods.
type CategoryPeriods struct {
	CategoryID uuid.UUID `json:"category_id"`
	Periods    int       `json:"periods"`
}

// CategoryPercent is a category's cashback percent.
type CategoryPercent struct {
	CategoryID uuid.UUID       `json:"category_id"`
	Percent    decimal.Decimal `json:"percent"`
}

// Named says whether the card has cashback rules at all.
func (c Cashback) Named() bool {
	return c.BasePercent.IsPositive() || len(c.Categories) > 0
}

// Fees are what the tariff charges besides interest (decision Р-30), told
// before they are charged; the journal gets them when the bank takes them.
// Amounts in minor units, percents of the money moved; PenaltyDaily is a
// percent of the payment missed, a day, PenaltyYearly a year (Т-Банк).
type Fees struct {
	Monthly int64
	// Yearly is charged at the statement after the card's first spending and
	// at every twelfth one after it (Т-Банк).
	Yearly      int64
	CashFree    int64
	CashPercent decimal.Decimal
	CashFixed   int64
	// TransferFree is what a statement period moves off the card without a
	// fee (Т-Банк: 80 000 ₽).
	TransferFree    int64
	TransferPercent decimal.Decimal
	TransferFixed   int64
	// IntroDays from the contract's day, cash and transfers are free up to
	// IntroFree in all (ВТБ: 50 000 ₽ in the first 30 days).
	IntroDays     int
	IntroFree     int64
	PenaltyDaily  decimal.Decimal
	PenaltyYearly decimal.Decimal
	// PenaltyFromDay is the day of a payment's lateness the penalty runs
	// from («Халва»: the 6th); 0 or 1 for the first.
	PenaltyFromDay int
}

// Cash is the fee for taking amount out in cash with already taken out this
// period and introLeft of the first days' free part unused: the fixed part
// and the percent of what goes past the free parts.
func (f Fees) Cash(amount, already, introLeft int64) int64 {
	over := amount - introLeft - max(f.CashFree-already, 0)
	if over <= 0 || f.CashPercent.IsZero() && f.CashFixed == 0 {
		return 0
	}
	return percentOf(over, f.CashPercent) + f.CashFixed
}

// Transfer is the fee for moving amount off the card, likewise against the
// period's free transfers.
func (f Fees) Transfer(amount, already, introLeft int64) int64 {
	over := amount - introLeft - max(f.TransferFree-already, 0)
	if over <= 0 || f.TransferPercent.IsZero() && f.TransferFixed == 0 {
		return 0
	}
	return percentOf(over, f.TransferPercent) + f.TransferFixed
}

// penalty is the penalty on overdue, the sum of what was overdue each day
// it runs.
func (f Fees) penalty(overdue int64) int64 {
	v, _ := money.Minor(decimal.NewFromInt(overdue).Mul(f.PenaltyDaily.Add(f.PenaltyYearly.Div(decimal.NewFromInt(365)))).Div(hundred).Round(0))
	return v
}

func percentOf(amount int64, pct decimal.Decimal) int64 {
	v, _ := money.Minor(decimal.NewFromInt(amount).Mul(pct).Div(hundred).Round(0))
	return v
}

var hundred, thousand = decimal.NewFromInt(100), decimal.NewFromInt(1000)

// Validate refuses terms no statement can be worked out from.
func (t Terms) Validate() error {
	rate := func(r decimal.Decimal) bool { return !r.IsNegative() && r.LessThan(thousand) }
	share := func(r decimal.Decimal) bool { return !r.IsNegative() && r.LessThan(hundred) }
	switch {
	case t.Limit < 0:
		return fmt.Errorf("%w: the limit cannot be below zero", family.ErrValidation)
	case t.StatementDay < 1 || t.StatementDay > 31:
		return fmt.Errorf("%w: the statement day is 1 to 31", family.ErrValidation)
	case t.PaymentDays < 0 || t.PaymentDays > 60:
		return fmt.Errorf("%w: the days to pay after a statement are 0 to 60", family.ErrValidation)
	case t.GraceKind != FromStatement && t.GraceKind != Long && t.GraceKind != Windows && t.GraceKind != Running:
		return fmt.Errorf("%w: the grace runs from the statement, long, in windows or from the first purchase", family.ErrValidation)
	case (t.GraceKind == Long || t.GraceKind == Running) && (t.GraceDays < 1 || t.GraceDays > 1100):
		return fmt.Errorf("%w: a long grace is 1 to 1100 days", family.ErrValidation)
	case t.GraceKind == Running && t.RunFrom != FromPurchase && t.RunFrom != FromNextDay && t.RunFrom != FromMonthStart:
		return fmt.Errorf("%w: a grace from the first purchase starts on its day, the next one, or the 1st of its month", family.ErrValidation)
	case t.GraceKind == Windows && (t.WindowMonths < 1 || t.WindowMonths > 12):
		return fmt.Errorf("%w: a window is 1 to 12 periods of purchases", family.ErrValidation)
	case t.GraceKind == Windows && (t.GraceMonths < t.WindowMonths || t.GraceMonths > 36):
		return fmt.Errorf("%w: a window's purchases are paid by the end of its own last period or a later one, at most the 36th", family.ErrValidation)
	case t.GracePeriods < 0 || t.GracePeriods > 12 || slices.ContainsFunc(t.GraceCategories, func(c CategoryPeriods) bool { return c.Periods < 0 || c.Periods > 12 }):
		return fmt.Errorf("%w: purchases are paid 0 to 12 statements later", family.ErrValidation)
	case t.GraceKind != FromStatement && (t.GracePeriods > 0 || len(t.GraceCategories) > 0):
		return fmt.Errorf("%w: statements later is a grace from the statement's", family.ErrValidation)
	case t.GraceToMonthEnd && t.GraceKind != Long && t.GraceKind != Running:
		return fmt.Errorf("%w: a grace to its month's end is a long one or one from the first purchase", family.ErrValidation)
	case t.GraceKind == Windows && t.OpenedOn == nil:
		return fmt.Errorf("%w: windows count from the day the card's contract was made", family.ErrValidation)
	case t.MinPercent.IsNegative() || t.MinPercent.GreaterThan(hundred):
		return fmt.Errorf("%w: the minimum payment is 0 to 100 percent of the debt", family.ErrValidation)
	case t.PayDay < 0 || t.PayDay > 31 || t.PayDay > 0 && t.PayByPeriodEnd:
		return fmt.Errorf("%w: the minimum is due by a day of the month (1 to 31) or by the period's end, not both", family.ErrValidation)
	case t.MinRoundUp < 0:
		return fmt.Errorf("%w: the minimum's rounding cannot be below zero", family.ErrValidation)
	case t.MinFloor < 0:
		return fmt.Errorf("%w: the smallest minimum payment cannot be below zero", family.ErrValidation)
	case !rate(t.AnnualRate):
		return fmt.Errorf("%w: the card's rate is from 0 to 999.9999 percent", family.ErrValidation)
	case t.OwnRate != nil && !rate(*t.OwnRate):
		return fmt.Errorf("%w: the own money's rate is from 0 to 999.9999 percent", family.ErrValidation)
	case t.Fees.Monthly < 0 || t.Fees.Yearly < 0 || t.Fees.CashFree < 0 || t.Fees.CashFixed < 0 || t.Fees.TransferFree < 0 ||
		t.Fees.TransferFixed < 0 || t.Fees.IntroFree < 0:
		return fmt.Errorf("%w: the tariff's amounts cannot be below zero", family.ErrValidation)
	case !share(t.Fees.CashPercent) || !share(t.Fees.TransferPercent):
		return fmt.Errorf("%w: a fee is 0 to 99.999 percent", family.ErrValidation)
	case t.Fees.IntroDays < 0 || t.Fees.IntroDays > 366 || (t.Fees.IntroDays > 0) != (t.Fees.IntroFree > 0):
		return fmt.Errorf("%w: free cash and transfers at the start are an amount for 1 to 366 days, both or neither", family.ErrValidation)
	case t.Fees.IntroDays > 0 && t.OpenedOn == nil:
		return fmt.Errorf("%w: the first days count from the day the card's contract was made", family.ErrValidation)
	case t.Fees.PenaltyDaily.IsNegative() || t.Fees.PenaltyDaily.GreaterThanOrEqual(decimal.NewFromInt(10)):
		return fmt.Errorf("%w: the penalty is 0 to 9.9999 percent a day", family.ErrValidation)
	case !rate(t.Fees.PenaltyYearly) || t.Fees.PenaltyYearly.IsPositive() && t.Fees.PenaltyDaily.IsPositive():
		return fmt.Errorf("%w: the penalty is 0 to 999.9999 percent a year, or a day, not both", family.ErrValidation)
	case t.Fees.PenaltyFromDay < 0 || t.Fees.PenaltyFromDay > 90:
		return fmt.Errorf("%w: the penalty runs from a day of lateness 1 to 90", family.ErrValidation)
	case !share(t.Cashback.BasePercent) || slices.ContainsFunc(t.Cashback.Categories, func(c CategoryPercent) bool { return !share(c.Percent) }):
		return fmt.Errorf("%w: a cashback is 0 to 99.999 percent", family.ErrValidation)
	case t.Installment.Validate() != nil:
		return t.Installment.Validate()
	case t.Cashback.MonthlyCap < 0 || t.Cashback.CreditDays < 0 || t.Cashback.CreditDays > 60:
		return fmt.Errorf("%w: the cashback's cap cannot be below zero, and it comes 0 to 60 days after the statement", family.ErrValidation)
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

// statementAfter is the statement n periods after the one on day s.
func (t Terms) statementAfter(s time.Time, n int) time.Time {
	m := time.Date(s.Year(), s.Month()+time.Month(n), 1, 0, 0, 0, 0, time.UTC)
	return t.statementOn(m.Year(), m.Month())
}

// dueOn is the day a statement on day s is to be paid by: PaymentDays after
// it, or the last day of the period it opens.
func (t Terms) dueOn(s time.Time) time.Time {
	switch {
	case t.PayByPeriodEnd:
		return t.statementAfter(s, 1).AddDate(0, 0, -1)
	case t.PayDay > 0:
		for m := 0; ; m++ {
			first := time.Date(s.Year(), s.Month()+time.Month(m), 1, 0, 0, 0, 0, time.UTC)
			last := first.AddDate(0, 1, -1).Day()
			if d := first.AddDate(0, 0, min(t.PayDay, last)-1); d.After(s) {
				return d
			}
		}
	}
	return s.AddDate(0, 0, t.PaymentDays)
}

// window is the window of periods a day's purchases belong to, counted from
// the period of OpenedOn.
func (t Terms) window(d time.Time) (start, end time.Time) {
	first, _ := t.period(*t.OpenedOn)
	at, _ := t.period(d)
	n := (at.Year()-first.Year())*12 + int(at.Month()-first.Month())
	k := n / t.WindowMonths
	if n < 0 && n%t.WindowMonths != 0 {
		k--
	}
	start = t.statementAfter(first, k*t.WindowMonths)
	return start, t.statementAfter(start, t.WindowMonths)
}

// group is the purchases that share a deadline with a day's: its period, or
// its window.
func (t Terms) group(d time.Time) (from, to time.Time) {
	if t.GraceKind == Windows {
		return t.window(d)
	}
	return t.period(d)
}

// deadline is the last day a purchase on day d is free of interest; periods
// are its statements later (GracePeriods).
func (t Terms) deadline(d time.Time, periods int) time.Time {
	start, end := t.period(d)
	switch t.GraceKind {
	case Long:
		return t.toMonthEnd(start.AddDate(0, 0, t.GraceDays-1))
	case Windows:
		start, _ = t.window(d)
		return t.statementAfter(start, t.GraceMonths).AddDate(0, 0, -1)
	}
	return t.dueOn(t.statementAfter(end, periods))
}

// toMonthEnd is a grace's last day, moved to its month's last with
// GraceToMonthEnd.
func (t Terms) toMonthEnd(d time.Time) time.Time {
	if !t.GraceToMonthEnd {
		return d
	}
	return time.Date(d.Year(), d.Month()+1, 0, 0, 0, 0, 0, time.UTC)
}

// periods is a purchase's statements later: its category's own, or the
// card's.
func (t Terms) periods(op operation.Operation, kinds Kinds) int {
	if op.CategoryID != nil {
		if n, ok := kinds.Graces[*op.CategoryID]; ok {
			return n
		}
	}
	return t.GracePeriods
}

// movedOff says whether a journal row is money moved off the card: cash taken
// out or a transfer to another account.
func movedOff(op operation.Operation, kinds Kinds) bool {
	return op.Type == operation.TypeWithdrawal && op.TransferGroupID != nil && op.AmountMinor < 0 && !kinds.charge(op)
}

// Due is an amount to pay by a day.
type Due struct {
	On     time.Time
	Amount int64
}

// Lost is the part of a period's (or a window's) purchases still owed after
// its grace ran out, and roughly the interest on it so far — the bank counts
// by its own rules; this is the yearly rate from each purchase's day. Early:
// the grace was taken off before its own deadline, by an earlier one missed
// (Terms.GraceAllLost).
type Lost struct {
	From, To time.Time
	Deadline time.Time
	Amount   int64
	Interest int64
	Early    bool
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
	// GraceOffSince is the day a missed deadline — or, GraceOffByMinimum, a
	// missed minimum — took the grace off the whole debt
	// (Terms.GraceAllLost); zero while the grace holds. ToRestore is what is
	// left to repay for the purchases to come to have it again: the
	// purchases, or after a missed minimum the whole debt.
	GraceOffSince     time.Time
	GraceOffByMinimum bool
	ToRestore         int64
	// CashThisPeriod is the cash taken out since the last statement, against
	// the tariff's free part (Fees.CashFree); TransfersThisPeriod the money
	// moved off the card otherwise, against Fees.TransferFree.
	CashThisPeriod      int64
	TransfersThisPeriod int64
	// IntroLeft is what is left of the first days' free cash and transfers
	// (Fees.IntroFree), until IntroUntil, its last day; zero time once they
	// are over or with none.
	IntroLeft  int64
	IntroUntil time.Time
	// YearlyFeeOn is the next statement the yearly fee comes with
	// (Fees.Yearly); zero time with none or before the first spending.
	YearlyFeeOn time.Time
	// Penalty is roughly what the bank charges for the minimum missed so
	// far, at Fees.PenaltyDaily or PenaltyYearly from PenaltyFromDay.
	Penalty int64
	// MinimumOverdue is the part of Minimum that is earlier minimums missed:
	// the bank adds them to the next one, and they are due at once.
	MinimumOverdue int64
	// Installments are the purchases in installments still being shown;
	// InstallmentsDue the parts shown and not yet paid — in Minimum.
	Installments    []Installment
	InstallmentsDue int64
	// Bank is what the bank itself says is due, while it is ahead (Work's
	// bank figures); nil when nothing it said is.
	Bank *BankView
	// CashbackExpected is the cashback this period's purchases so far bring
	// by the card's rules, capped; it comes on CashbackOn (zero time when
	// the card names no rules).
	CashbackExpected int64
	CashbackOn       time.Time
}

// item is one debit still (partly) owed: a purchase the grace covers, a
// charge of the bank's, or money moved off the card. A lost purchase is
// charged from its day.
type item struct {
	on       time.Time
	left     int64
	grace    bool
	charge   bool
	deadline time.Time
	lost     bool
	early    bool
	// from and to are the purchases it shares its deadline with: a period,
	// a window, or a running grace.
	from, to time.Time
}

// purchase says whether a journal row is spending the grace covers: a
// withdrawal that is not one half of a move to the family's own account, nor
// a charge of the bank's, nor of a category the bank takes for transfers.
// Fees, interest charged and money moved off the card are owed from day one.
func purchase(op operation.Operation, kinds Kinds) bool {
	return op.Type == operation.TypeWithdrawal && op.TransferGroupID == nil && !kinds.charge(op) &&
		(op.CategoryID == nil || !kinds.Transfers[*op.CategoryID])
}

// Work works out the card's status on today from its journal, rows in the
// card's currency only; kinds tell the bank's charges by their categories.
// Payments clear the oldest debt first. The journal is walked day by day, so
// that a deadline missed (with Terms.GraceAllLost) takes the grace off what
// was owed then and off the purchases made until the purchases are repaid.
func Work(t Terms, ops []operation.Operation, currency string, today time.Time, kinds Kinds) Status {
	rows := make([]operation.Operation, 0, len(ops))
	for _, op := range ops {
		if op.Currency == currency && op.AmountMinor != 0 && !op.OccurredOn.After(today) {
			rows = append(rows, op)
		}
	}
	slices.SortStableFunc(rows, func(a, b operation.Operation) int { return a.OccurredOn.Compare(b.OccurredOn) })

	var st Status
	st.LastStatement, st.NextStatement = t.period(today)
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
	owedOf := func(which func(*item) bool) int64 {
		var sum int64
		for _, it := range items {
			if which(it) {
				sum += it.left
			}
		}
		return sum
	}
	isPurchase := func(it *item) bool { return it.grace }
	isCharge := func(it *item) bool { return it.charge }
	anything := func(*item) bool { return true }
	// takeOff takes the grace off from day: off what is owed now, and off
	// the purchases to come until what restores it is repaid — the
	// purchases, or (all) the whole debt.
	off, all := false, false
	takeOff := func(day time.Time, byMinimum, wholeDebt bool) {
		if !off {
			off, st.GraceOffSince = true, day
		}
		st.GraceOffByMinimum = st.GraceOffByMinimum || byMinimum
		all = all || byMinimum || wholeDebt
		for _, it := range items {
			if it.grace && !it.lost && it.left > 0 {
				it.lost, it.early = true, true
			}
		}
	}
	// miss loses the purchases whose deadline is before day; with
	// GraceAllLost one missed takes the grace off the rest.
	miss := func(day time.Time) {
		var missed time.Time
		for _, it := range items {
			if it.grace && !it.lost && it.left > 0 && it.deadline.Before(day) {
				it.lost = true
				if missed.IsZero() || it.deadline.Before(missed) {
					missed = it.deadline
				}
			}
		}
		// A running grace missed is over until the debt is repaid in full.
		if (t.GraceAllLost || t.GraceKind == Running) && !missed.IsZero() {
			takeOff(missed.AddDate(0, 0, 1), false, t.GraceKind == Running)
		}
	}
	// The running grace the purchases now share, while the card is in debt.
	var run *item
	// The purchases in installments, kept apart from the grace; the parts
	// shown and not yet paid, which a payment goes to first.
	var plans []*Installment
	var partsUnpaid, partsAtLast, debtAtLast int64
	// The minimum of each statement on the way, and what was paid toward it,
	// for a minimum missed; what is overdue of the minimums missed, paid
	// first, and the penalty it runs up while overdue; the charges the last
	// statement showed, for ChargesInFull.
	var charges, minimumDue, paidToward, overdue, overdueDays int64
	var lateDays int
	var minimumBy, minimumFrom, periodLostUntil time.Time
	stated := false
	first := today
	if len(rows) > 0 {
		first = rows[0].OccurredOn
	}
	k := 0
	for day := first; !day.After(today); day = day.AddDate(0, 0, 1) {
		if !minimumBy.IsZero() && minimumBy.Before(day) {
			if paidToward < minimumDue {
				overdue += minimumDue - paidToward
				if t.GraceAllLost {
					takeOff(minimumBy.AddDate(0, 0, 1), true, true)
				}
				// The purchases its next statement shows lose their grace.
				if t.MissedMinimumPeriod {
					periodLostUntil = t.statementAfter(minimumFrom, 1)
					for _, it := range items {
						if it.grace && !it.lost && it.left > 0 && !it.on.Before(minimumFrom) && it.on.Before(periodLostUntil) {
							it.lost, it.early = true, true
						}
					}
				}
			}
			minimumBy = time.Time{}
		}
		if start, _ := t.period(day); start.Equal(day) {
			// The statement shows the next part of each installment.
			var parts int64
			for _, in := range plans {
				if in.Billed < in.Plan.Months && in.On.Before(day) {
					principal, fee := in.part()
					in.Billed++
					in.Left -= principal
					parts += principal + fee
				}
			}
			partsUnpaid += parts
			debt := owedOf(anything) - credit
			minimumDue, minimumBy, minimumFrom, paidToward = t.minimum(debt, owedOf(isCharge))+parts, t.dueOn(day), day, 0
			if day.Equal(st.LastStatement) {
				charges, stated = owedOf(isCharge), true
				partsAtLast, debtAtLast = parts, debt
			}
		}
		miss(day)
		// What restores the grace repaid, the purchases from the next day
		// keep it again.
		restored := false
		for ; k < len(rows) && rows[k].OccurredOn.Equal(day); k++ {
			op := rows[k]
			if op.AmountMinor > 0 {
				toParts := min(partsUnpaid, op.AmountMinor)
				partsUnpaid -= toParts
				pay(op.AmountMinor - toParts)
				toOverdue := min(overdue, op.AmountMinor)
				overdue -= toOverdue
				paidToward += op.AmountMinor - toOverdue
				restore := isPurchase
				if all {
					restore = anything
				}
				restored = restored || off && owedOf(restore) == 0
				// Repaid in full, the next purchase starts a new grace.
				if owedOf(anything) <= credit {
					run = nil
				}
				continue
			}
			owed := -op.AmountMinor
			take := min(credit, owed)
			credit -= take
			if plan, ok := installmentPlan(t, op, kinds); ok && owed > take {
				owed -= take
				plans = append(plans, &Installment{OperationID: op.ID, On: day, Amount: owed, Plan: plan, Left: owed, Note: op.Note})
				continue
			}
			if owed -= take; owed > 0 {
				it := &item{on: day, left: owed, grace: purchase(op, kinds) || t.GraceMoves && movedOff(op, kinds), charge: kinds.charge(op)}
				it.from, it.to = t.group(day)
				if it.grace {
					it.deadline = t.deadline(day, t.periods(op, kinds))
					if t.GraceKind == Running && !off {
						if run == nil {
							start := t.RunFrom.runStart(day)
							end := start.AddDate(0, 0, t.GraceDays)
							run = &item{from: start, to: end, deadline: t.toMonthEnd(end.AddDate(0, 0, -1))}
						}
						it.from, it.to, it.deadline = run.from, run.to, run.deadline
					}
					it.lost, it.early = off, off
					if day.Before(periodLostUntil) {
						it.lost, it.early = true, true
					}
				}
				items = append(items, it)
			}
		}
		if restored {
			off, all, st.GraceOffSince, st.GraceOffByMinimum = false, false, time.Time{}, false
			run = nil
		}
		// The penalty of the stretch overdue now, from its PenaltyFromDay-th
		// day; one repaid is the bank's charge by then, in the journal.
		if overdue > 0 {
			if lateDays++; lateDays >= t.Fees.PenaltyFromDay {
				overdueDays += overdue
			}
		} else {
			overdueDays, lateDays = 0, 0
		}
	}
	st.Penalty = t.Fees.penalty(overdueDays)
	if !stated {
		charges = owedOf(isCharge)
	}
	if off {
		st.ToRestore = owedOf(isPurchase)
		if all {
			st.ToRestore = max(owedOf(anything)-credit, 0)
		}
	}

	for _, it := range items {
		st.Debt += it.left
	}
	st.Debt -= credit
	st.InstallmentsDue = partsUnpaid
	var nextParts int64
	for _, in := range plans {
		st.Debt += in.Left
		if in.Left > 0 {
			principal, fee := in.part()
			in.Next = principal + fee
			if in.On.Before(st.NextStatement) {
				nextParts += in.Next
			}
			st.Installments = append(st.Installments, *in)
		}
	}
	st.Debt += partsUnpaid
	st.Available = t.Limit - max(st.Debt, 0)

	rate := t.AnnualRate.Div(hundred)
	interest := func(amount int64, from time.Time) int64 {
		days := int64(today.Sub(from).Hours() / 24)
		v, _ := money.Minor(decimal.NewFromInt(amount).Mul(rate).Mul(decimal.NewFromInt(days)).Div(decimal.NewFromInt(365)))
		return v
	}
	grace := map[time.Time]int64{}
	type lostKey struct{ from, deadline time.Time }
	lost := map[lostKey]*Lost{}
	for _, it := range items {
		if it.left == 0 {
			continue
		}
		if !it.grace {
			st.NonGrace += it.left
			st.NonGraceInterest += interest(it.left, it.on)
			continue
		}
		if !it.lost {
			grace[it.deadline] += it.left
			continue
		}
		key := lostKey{it.from, it.deadline}
		l, ok := lost[key]
		if !ok {
			l = &Lost{From: it.from, To: it.to.AddDate(0, 0, -1), Deadline: it.deadline, Early: true}
			lost[key] = l
		}
		l.Early = l.Early && it.early
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
	slices.SortFunc(st.Lost, func(a, b Lost) int {
		if c := a.From.Compare(b.From); c != 0 {
			return c
		}
		return a.Deadline.Compare(b.Deadline)
	})

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
	// The cash out since the statement. The monthly fee is charged at a
	// period's end: while the journal has no charge of the bank's in the
	// period before the statement (or in this one, for the next statement),
	// the fee is taken to be still to come into it.
	previous := t.statementAfter(st.LastStatement, -1)
	var chargedBefore, chargedNow bool
	for _, op := range rows {
		if op.AmountMinor >= 0 || op.OccurredOn.Before(previous) {
			continue
		}
		now := !op.OccurredOn.Before(st.LastStatement)
		switch {
		case now && kinds.Cash[op.ID]:
			st.CashThisPeriod -= op.AmountMinor
		case now && op.TransferGroupID != nil:
			st.TransfersThisPeriod -= op.AmountMinor
		}
		if kinds.charge(op) {
			chargedBefore, chargedNow = chargedBefore || !now, chargedNow || now
		}
	}
	st.intro(t, rows, today)
	yearlyLast, yearlyNext := st.yearly(t, rows, kinds)
	st.cashback(t, rows, kinds)
	var fee int64
	if !chargedBefore && first.Before(st.LastStatement) {
		fee = t.Fees.Monthly
	}
	if yearlyLast && !chargedBefore && !chargedNow {
		fee += t.Fees.Yearly
	}
	st.MinimumOn = t.dueOn(st.LastStatement)
	// Since the statement, payments go to the minimums missed first; the
	// statement's debt is the walk's, purchases in installments apart.
	paid, base := paidSince, atStatement
	if stated {
		paid, base = paidToward, debtAtLast
	}
	if minimum := t.minimum(base+fee, charges+fee) + partsAtLast; minimum > paid {
		st.Minimum = minimum - paid
		st.MinimumMissed = st.MinimumOn.Before(today)
	}
	switch {
	case st.MinimumMissed:
		// Past its day, this one is among the missed already.
		st.Minimum, st.MinimumOverdue = max(st.Minimum, overdue), overdue
	case overdue > 0:
		st.Minimum += overdue
		st.MinimumOverdue = overdue
	}
	if st.Minimum == 0 && st.MinimumOn.Before(today) {
		fee = 0
		if !chargedNow {
			fee = t.Fees.Monthly
		}
		if yearlyNext {
			fee += t.Fees.Yearly
		}
		st.nextMinimum(t, st.Debt-instLeft(plans)-partsUnpaid+fee, owedOf(isCharge)+fee)
		st.Minimum += nextParts
	}
	return st
}

// installmentPlan is the plan a purchase is in, if any: its own, or the
// card's for every purchase.
func installmentPlan(t Terms, op operation.Operation, kinds Kinds) (Plan, bool) {
	if op.Type != operation.TypeWithdrawal || op.TransferGroupID != nil || kinds.charge(op) {
		return Plan{}, false
	}
	if p, ok := kinds.Installments[op.ID]; ok && p.Months > 0 {
		return p, true
	}
	if t.Installment.Months > 0 && purchase(op, kinds) {
		return t.Installment, true
	}
	return Plan{}, false
}

// instLeft is what of the installments' sums is still to be shown.
func instLeft(plans []*Installment) int64 {
	var sum int64
	for _, in := range plans {
		sum += in.Left
	}
	return sum
}

// intro is what is left of the free cash and transfers of the card's first
// days (Fees.IntroFree), while they last: all the money moved off the card
// since the contract's day counts against it.
func (st *Status) intro(t Terms, rows []operation.Operation, today time.Time) {
	if t.Fees.IntroDays == 0 || t.OpenedOn == nil {
		return
	}
	until := t.OpenedOn.AddDate(0, 0, t.Fees.IntroDays-1)
	if today.After(until) {
		return
	}
	var used int64
	for _, op := range rows {
		if op.TransferGroupID != nil && op.AmountMinor < 0 && !op.OccurredOn.Before(*t.OpenedOn) {
			used -= op.AmountMinor
		}
	}
	st.IntroLeft, st.IntroUntil = max(t.Fees.IntroFree-used, 0), until
}

// yearly finds the next statement the yearly fee comes with — the one after
// the journal's first spending, and every twelfth after it — and says
// whether the last statement and the next one are such.
func (st *Status) yearly(t Terms, rows []operation.Operation, kinds Kinds) (last, next bool) {
	if t.Fees.Yearly == 0 {
		return false, false
	}
	i := slices.IndexFunc(rows, func(op operation.Operation) bool { return op.AmountMinor < 0 && !kinds.charge(op) })
	if i < 0 {
		return false, false
	}
	_, firstFee := t.period(rows[i].OccurredOn)
	months := func(s time.Time) int {
		return (s.Year()-firstFee.Year())*12 + int(s.Month()-firstFee.Month())
	}
	if n := months(st.LastStatement); n >= 0 && n%12 == 0 {
		last = true
	}
	n := max(months(st.NextStatement), 0)
	st.YearlyFeeOn = t.statementAfter(firstFee, (n+11)/12*12)
	return last, st.YearlyFeeOn.Equal(st.NextStatement)
}

// cashback is what this period's purchases bring by the card's rules: each
// at its category's percent or the base one, whichever is higher, the sum not
// past the cap.
func (st *Status) cashback(t Terms, rows []operation.Operation, kinds Kinds) {
	if !t.Cashback.Named() {
		return
	}
	st.CashbackOn = st.NextStatement.AddDate(0, 0, t.Cashback.CreditDays)
	var sum decimal.Decimal
	for _, op := range rows {
		if op.OccurredOn.Before(st.LastStatement) || op.AmountMinor >= 0 || !purchase(op, kinds) {
			continue
		}
		pct := t.Cashback.BasePercent
		if op.CategoryID != nil {
			if own, ok := kinds.Earns[*op.CategoryID]; ok && own.GreaterThan(pct) {
				pct = own
			}
		}
		sum = sum.Add(decimal.NewFromInt(-op.AmountMinor).Mul(pct).Div(hundred))
	}
	st.CashbackExpected, _ = money.Minor(sum.Floor())
	if t.Cashback.MonthlyCap > 0 {
		st.CashbackExpected = min(st.CashbackExpected, t.Cashback.MonthlyCap)
	}
}

// nextMinimum is the next statement's minimum, from the debt it will show
// and the charges in it.
func (st *Status) nextMinimum(t Terms, debt, charges int64) {
	st.MinimumOn = t.dueOn(st.NextStatement)
	st.Minimum = t.minimum(debt, charges)
	st.MinimumEstimate = true
}

// minimum is the minimum payment on a debt: MinPercent of it, not less than
// MinFloor — of the debt less the charges, and the charges on top, with
// ChargesInFull — never more than the debt itself.
func (t Terms) minimum(debt, charges int64) int64 {
	if debt <= 0 {
		return 0
	}
	if !t.ChargesInFull {
		charges = 0
	}
	charges = min(max(charges, 0), debt)
	share, _ := money.Minor(decimal.NewFromInt(debt - charges).Mul(t.MinPercent).Div(hundred))
	minimum := max(share, t.MinFloor) + charges
	if r := t.MinRoundUp; r > 0 && minimum%r != 0 {
		minimum += r - minimum%r
	}
	return min(minimum, debt)
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
	st.MinimumOn = t.dueOn(st.LastStatement)
	st.Minimum = t.minimum(st.Debt, 0)
	st.MinimumEstimate = true
	if st.MinimumOn.Before(today) {
		st.nextMinimum(t, st.Debt, 0)
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

// Kinds sorts the card's rows: categories that mean cashback, categories
// that mean a charge (interest on credit, bank fees), and — the card's own —
// those the bank takes for transfers (Terms.TransferCategories, with their
// subcategories), and the rows that took cash out (moved to a cash account).
type Kinds struct {
	Cashback  map[uuid.UUID]bool
	Charges   map[uuid.UUID]bool
	Transfers map[uuid.UUID]bool
	Cash      map[uuid.UUID]bool
	// Earns is the card's cashback percent by category, its subcategories
	// with it (Terms.Cashback.Categories).
	Earns map[uuid.UUID]decimal.Decimal
	// Installments are the card's purchases in installments by their row,
	// over Terms.Installment.
	Installments map[uuid.UUID]Plan
	// Graces are the card's own statements later by category, its
	// subcategories with it (Terms.GraceCategories).
	Graces map[uuid.UUID]int
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
