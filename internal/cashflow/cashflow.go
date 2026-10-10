// Package cashflow is the family's money report (household stage 1, decision
// Р-24): what came in and went out over a period, by category and by month, in
// the space's base currency at the rate of each row's day. It reads the
// journal, the accounts and the categories through their modules and owns no
// table.
//
// A row lands in one place:
//   - filed under a category: income or spending by that category, on any
//     account (a withdrawal from a broker filed as «Путешествия» is a trip);
//   - unfiled on a brokerage account: the investments block — money put in,
//     taken out, paid out by papers, interest, the broker's fees and taxes —
//     which is neither earning nor spending of the family;
//   - unfiled on any other account: interest earns, a fee or a tax spends
//     under a group of its own, and a deposit or a withdrawal waits in
//     «не разнесено» until somebody files it.
//
// A fee charged on top of a row (fee_minor) spends under «комиссии», or is an
// investment cost on a broker's account. A transfer between the family's own
// accounts is none of this and is not read.
package cashflow

import (
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/operation"
)

// Group names a line that is not a category: unfiled interest, fees and taxes
// off a broker's account.
type Group string

const (
	GroupInterest Group = "interest"
	GroupFee      Group = "fee"
	GroupTax      Group = "tax"
)

// Flow is an amount over the period and by month, in minor units of the base
// currency; spending is counted positive.
type Flow struct {
	Total   int64
	ByMonth []int64
}

func (f *Flow) add(month int, amount int64) {
	f.Total += amount
	f.ByMonth[month] += amount
}

// Line is one category (with the ones under it that moved money) or one group.
// Direct is what was filed under the category itself rather than under one of
// its children; on a child it equals the line.
type Line struct {
	CategoryID *uuid.UUID
	Group      Group
	Flow
	Direct   Flow
	Children []Line
}

// Section is income or spending: its lines in the categories' order, groups
// last, and their sum.
type Section struct {
	Flow
	Lines []Line
}

// Investments is the money between the family and its brokers.
type Investments struct {
	Deposited, Withdrawn, Payouts, Interest, Costs Flow
}

// Report is the period's money.
type Report struct {
	BaseCurrency string
	From, To     time.Time
	Months       []time.Time // the first day of each month the period touches
	Income       Section
	Expense      Section
	UnfiledIn    Flow
	UnfiledOut   Flow
	Investments  Investments
	// MissingRates are the currencies of rows left out for want of a rate on
	// their day; LeftOut counts those rows.
	MissingRates []string
	LeftOut      int
}

// entry is a row with its money in the base currency.
type entry struct {
	op     operation.Operation
	amount int64 // amount_minor converted, sign kept
	fee    int64 // fee_minor converted, positive
}

// months lists the first day of each month from from's to to's.
func months(from, to time.Time) []time.Time {
	var out []time.Time
	for m := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC); !m.After(to); m = m.AddDate(0, 1, 0) {
		out = append(out, m)
	}
	return out
}

func monthIndex(from, day time.Time) int {
	return (day.Year()-from.Year())*12 + int(day.Month()) - int(from.Month())
}

// assemble lays the entries out. accounts are the ones the report covers;
// an entry on any other account is not counted. categories are the space's,
// in their order.
func assemble(r *Report, entries []entry, accounts map[uuid.UUID]account.Account, categories []category.Category) {
	n := len(r.Months)
	flow := func() Flow { return Flow{ByMonth: make([]int64, n)} }
	r.Income.Flow, r.Expense.Flow = flow(), flow()
	r.UnfiledIn, r.UnfiledOut = flow(), flow()
	r.Investments = Investments{Deposited: flow(), Withdrawn: flow(), Payouts: flow(), Interest: flow(), Costs: flow()}

	byCategory := map[uuid.UUID]category.Category{}
	for _, c := range categories {
		byCategory[c.ID] = c
	}
	// Each category's own flow and each group's, filled as rows are read and
	// arranged into lines at the end.
	filed := map[uuid.UUID]*Flow{}
	groups := map[Group]*Flow{}
	into := func(m map[uuid.UUID]*Flow, id uuid.UUID) *Flow {
		if m[id] == nil {
			f := flow()
			m[id] = &f
		}
		return m[id]
	}
	group := func(g Group) *Flow {
		if groups[g] == nil {
			f := flow()
			groups[g] = &f
		}
		return groups[g]
	}

	for _, e := range entries {
		acc, ok := accounts[e.op.AccountID]
		if !ok {
			continue
		}
		month := monthIndex(r.Months[0], e.op.OccurredOn)
		broker := acc.Type == account.TypeBrokerage
		if e.fee != 0 {
			if broker {
				r.Investments.Costs.add(month, e.fee)
			} else {
				group(GroupFee).add(month, e.fee)
				r.Expense.add(month, e.fee)
			}
		}
		if c, ok := categoryOf(e.op, byCategory); ok {
			if c.Kind == category.KindIncome {
				into(filed, c.ID).add(month, e.amount)
				r.Income.add(month, e.amount)
			} else {
				into(filed, c.ID).add(month, -e.amount)
				r.Expense.add(month, -e.amount)
			}
			continue
		}
		switch {
		case broker || e.op.Type == operation.TypeDividend || e.op.Type == operation.TypeCoupon:
			inv := &r.Investments
			switch e.op.Type {
			case operation.TypeDeposit:
				inv.Deposited.add(month, e.amount)
			case operation.TypeWithdrawal:
				inv.Withdrawn.add(month, -e.amount)
			case operation.TypeDividend, operation.TypeCoupon:
				inv.Payouts.add(month, e.amount)
			case operation.TypeInterest:
				inv.Interest.add(month, e.amount)
			case operation.TypeFee, operation.TypeTax:
				inv.Costs.add(month, -e.amount)
			}
		case e.op.Type == operation.TypeDeposit:
			r.UnfiledIn.add(month, e.amount)
		case e.op.Type == operation.TypeWithdrawal:
			r.UnfiledOut.add(month, -e.amount)
		case e.op.Type == operation.TypeInterest:
			group(GroupInterest).add(month, e.amount)
			r.Income.add(month, e.amount)
		case e.op.Type == operation.TypeFee:
			group(GroupFee).add(month, -e.amount)
			r.Expense.add(month, -e.amount)
		case e.op.Type == operation.TypeTax:
			group(GroupTax).add(month, -e.amount)
			r.Expense.add(month, -e.amount)
		}
	}

	r.Income.Lines = lines(categories, category.KindIncome, filed, flow)
	r.Expense.Lines = lines(categories, category.KindExpense, filed, flow)
	if f := groups[GroupInterest]; f != nil {
		r.Income.Lines = append(r.Income.Lines, Line{Group: GroupInterest, Flow: *f, Direct: *f})
	}
	for _, g := range []Group{GroupFee, GroupTax} {
		if f := groups[g]; f != nil {
			r.Expense.Lines = append(r.Expense.Lines, Line{Group: g, Flow: *f, Direct: *f})
		}
	}
}

// categoryOf is the row's category when it names one the report knows.
func categoryOf(op operation.Operation, byCategory map[uuid.UUID]category.Category) (category.Category, bool) {
	if op.CategoryID == nil {
		return category.Category{}, false
	}
	c, ok := byCategory[*op.CategoryID]
	return c, ok
}

// lines arranges one kind's filed flows as the tree: a top-level category with
// whatever moved under it, children in their order. Categories nothing was
// filed under are left out.
func lines(categories []category.Category, kind category.Kind, filed map[uuid.UUID]*Flow, flow func() Flow) []Line {
	var out []Line
	for _, top := range categories {
		if top.Kind != kind || top.ParentID != nil {
			continue
		}
		line := Line{CategoryID: &top.ID, Flow: flow(), Direct: flow()}
		used := false
		if f := filed[top.ID]; f != nil {
			line.Direct = *f
			line.merge(*f)
			used = true
		}
		for _, child := range categories {
			if child.ParentID == nil || *child.ParentID != top.ID {
				continue
			}
			f := filed[child.ID]
			if f == nil {
				continue
			}
			line.Children = append(line.Children, Line{CategoryID: &child.ID, Flow: *f, Direct: *f})
			line.merge(*f)
			used = true
		}
		if used {
			out = append(out, line)
		}
	}
	return out
}

func (f *Flow) merge(g Flow) {
	f.Total += g.Total
	for i, v := range g.ByMonth {
		f.ByMonth[i] += v
	}
}
