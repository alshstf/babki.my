// Package loan keeps a loan's terms and works out its schedule (household
// stage 2, decision Р-24): each month's payment, the interest in it — a
// spending — and the part that pays the debt down — a transfer to the loan's
// account. The journal stays the record of what was paid; the schedule is what
// was agreed.
package loan

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/money"
)

// Kind is how a loan is repaid.
type Kind string

const (
	// Annuity is equal monthly payments, interest first.
	Annuity Kind = "annuity"
	// Differentiated is an equal part of the debt each month plus the
	// interest on what is left.
	Differentiated Kind = "differentiated"
)

// Terms are a loan's agreed terms. Principal is in minor units of the loan
// account's currency; AnnualRate in percent.
type Terms struct {
	AccountID  uuid.UUID
	Principal  int64
	AnnualRate decimal.Decimal
	TermMonths int
	IssuedOn   time.Time
	Kind       Kind
}

// Validate refuses terms no schedule can be worked out from.
func (t Terms) Validate() error {
	switch {
	case t.Principal <= 0:
		return fmt.Errorf("%w: the amount borrowed must be positive", family.ErrValidation)
	case t.AnnualRate.IsNegative() || t.AnnualRate.GreaterThanOrEqual(decimal.NewFromInt(1000)):
		return fmt.Errorf("%w: the yearly rate is from 0 to 999.9999 percent", family.ErrValidation)
	case t.TermMonths < 1 || t.TermMonths > 600:
		return fmt.Errorf("%w: the term is 1 to 600 months", family.ErrValidation)
	case t.Kind != Annuity && t.Kind != Differentiated:
		return fmt.Errorf("%w: a loan is repaid by annuity or differentiated payments", family.ErrValidation)
	case t.IssuedOn.IsZero():
		return fmt.Errorf("%w: the day the loan was issued is required", family.ErrValidation)
	}
	return nil
}

// Row is one month of the schedule, in minor units: the payment, the
// interest and the debt repaid in it, and the debt left after it.
type Row struct {
	On        time.Time
	Payment   int64
	Interest  int64
	Principal int64
	Left      int64
}

// Schedule works the terms out month by month: the first payment a month after
// the loan was issued. Interest is a twelfth of the yearly rate on the debt
// left; the last payment takes whatever rounding left over, so the debt ends
// at nought.
func Schedule(t Terms) ([]Row, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	monthly := t.AnnualRate.Div(decimal.NewFromInt(1200))
	left := decimal.NewFromInt(t.Principal)
	var payment decimal.Decimal
	if t.Kind == Annuity {
		if monthly.IsZero() {
			payment = left.Div(decimal.NewFromInt(int64(t.TermMonths)))
		} else {
			// P·r / (1 − (1 + r)^−n)
			growth := decimal.NewFromInt(1).Add(monthly).Pow(decimal.NewFromInt(int64(t.TermMonths)))
			payment = left.Mul(monthly).Mul(growth).Div(growth.Sub(decimal.NewFromInt(1)))
		}
		payment = payment.Round(0)
	}
	part := decimal.NewFromInt(t.Principal).Div(decimal.NewFromInt(int64(t.TermMonths)))
	rows := make([]Row, 0, t.TermMonths)
	for i := 1; i <= t.TermMonths; i++ {
		interest := left.Mul(monthly).Round(0)
		var principal decimal.Decimal
		if t.Kind == Annuity {
			principal = payment.Sub(interest)
		} else {
			principal = part.Round(0)
		}
		if i == t.TermMonths || principal.GreaterThan(left) {
			principal = left
		}
		left = left.Sub(principal)
		row := Row{On: t.IssuedOn.AddDate(0, i, 0)}
		var err error
		if row.Interest, err = money.Minor(interest); err != nil {
			return nil, err
		}
		if row.Principal, err = money.Minor(principal); err != nil {
			return nil, err
		}
		if row.Left, err = money.Minor(left); err != nil {
			return nil, err
		}
		row.Payment = row.Interest + row.Principal
		rows = append(rows, row)
	}
	return rows, nil
}
