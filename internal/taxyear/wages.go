package taxyear

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/portfolio"
)

type flows interface {
	ListMoneyFlows(ctx context.Context, spaceID uuid.UUID, from, to time.Time) ([]portfolio.Operation, error)
}

type categories interface {
	List(ctx context.Context, spaceID uuid.UUID) ([]category.Category, error)
}

// WithEarnings lets the estimate read the earnings the family files under
// categories marked for tax: wages and deposits' interest (decision Р-41).
func (s *Service) WithEarnings(f flows, c categories) *Service {
	s.flows, s.categories = f, c
	return s
}

// Employer is one employer's part of a person's wages: what reached the
// account, and the gross and the tax withheld the program reckons from it.
type Employer struct {
	Name                 string
	Net, Gross, Withheld int64
}

// earning is one earning row, or part of one, filed under a marked category;
// unvalued when it is not in the tax currency.
type earning struct {
	person   uuid.UUID
	mark     category.TaxMark
	employer string
	minor    int64
	unvalued bool
}

// earnings are the year's earnings filed under marked categories, each with
// whose it is — the member who earned it, else the account's person; one not
// in the tax currency is marked unvalued.
func (s *Service) earnings(ctx context.Context, spaceID uuid.UUID, year int, currency string, accounts map[uuid.UUID]account.Account, owner uuid.UUID) ([]earning, error) {
	if s.flows == nil || s.categories == nil {
		return nil, nil
	}
	cats, err := s.categories.List(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	marks := map[uuid.UUID]category.TaxMark{}
	for _, c := range cats {
		if c.TaxMark != nil {
			marks[c.ID] = *c.TaxMark
		}
	}
	if len(marks) == 0 {
		return nil, nil
	}
	ops, err := s.flows.ListMoneyFlows(ctx, spaceID,
		time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC), time.Date(year, time.December, 31, 0, 0, 0, 0, time.UTC))
	if err != nil {
		return nil, err
	}
	var out []earning
	for _, o := range ops {
		if o.Type != portfolio.TypeDeposit {
			continue
		}
		type piece struct {
			cat   uuid.UUID
			minor int64
		}
		var pieces []piece
		switch {
		case len(o.Parts) > 0:
			for _, p := range o.Parts {
				pieces = append(pieces, piece{p.CategoryID, p.Amount})
			}
		case o.CategoryID != nil:
			pieces = append(pieces, piece{*o.CategoryID, o.AmountMinor})
		}
		for _, p := range pieces {
			mark, ok := marks[p.cat]
			if !ok {
				continue
			}
			who := whose(accounts[o.AccountID], owner)
			if o.MemberID != nil {
				who = *o.MemberID
			}
			out = append(out, earning{person: who, mark: mark, employer: strings.TrimSpace(o.Counterparty), minor: p.minor, unvalued: o.Currency != currency})
		}
	}
	return out, nil
}

// grossOf is the gross whose net, after the employer withholds by the bands
// on top of what it paid the person before this year, is net: each employer
// withholds on what it paid alone, the bands climbing over the year.
func grossOf(bands []family.TaxBand, before, net int64) int64 {
	lo, hi := net, 2*net+1
	for lo < hi {
		mid := lo + (hi-lo)/2
		if mid-(family.Banded(bands, before+mid, 1)-family.Banded(bands, before, 1)) < net {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// wagesOf sums a person's wages by employer, in the order they came; what
// each employer withheld is its tax on what it paid, in whole units as the
// tax is.
func wagesOf(bands []family.TaxBand, unit int64, rows []earning) []Employer {
	var out []Employer
	at := map[string]int{}
	for _, r := range rows {
		i, ok := at[r.employer]
		if !ok {
			i = len(out)
			at[r.employer] = i
			out = append(out, Employer{Name: r.employer})
		}
		e := &out[i]
		e.Gross += grossOf(bands, e.Gross, r.minor)
		e.Net += r.minor
	}
	for i := range out {
		out[i].Withheld = family.Banded(bands, out[i].Gross, unit)
	}
	return out
}
